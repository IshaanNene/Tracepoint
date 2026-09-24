package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IshaanNene/Tracepoint/internal/ops"
)

const serverToken = "e2e-token-0123456789"

// agentConfig is inline configuration an agent would send.
func agentConfig(url string) string {
	return `
version: 1
run: { duration: 2s, bucket: 500ms, seed: 9, timeout: 2s }
slo: { http: { p99: 2s } }
http:
  base_url: "` + url + `"
  executor: { rate: 20, max_in_flight: 8 }
  requests: [{ name: items, url: "/api/items" }]
`
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// serverCmd is the real binary as a client would launch it, with a clean environment.
func serverCmd(ctx context.Context, t *testing.T, args ...string) *osexec.Cmd {
	t.Helper()
	cmd := osexec.CommandContext(ctx, binary(t), args...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + cmd.Dir}
	return cmd
}

// The whole agent loop through `tracepoint serve`, with runs detached into their own
// processes exactly as in production.
func TestAgentLoopThroughServe(t *testing.T) {
	target := newServer(t, 0)
	addr := freeAddr(t)
	root := filepath.Join(t.TempDir(), "runs")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := serverCmd(ctx, t, "serve", "--listen", addr, "--token", serverToken, "--run-root", root)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		// Wait reports the cancellation itself; a clean shutdown on SIGINT exits 0.
		_ = cmd.Wait()
		if code := cmd.ProcessState.ExitCode(); code != 0 {
			t.Errorf("serve exited %d:\n%s", code, stderr.String())
		}
	}()

	client := &http.Client{Timeout: 70 * time.Second}
	post := func(op string, in any) (int, []byte) {
		t.Helper()
		b, _ := json.Marshal(in)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/v1/ops/"+op, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+serverToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v\n%s", op, err, stderr.String())
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	// Wait for it to listen.
	for deadline := time.Now().Add(15 * time.Second); ; {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/healthz", http.NoBody)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never listened:\n%s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	cfg := agentConfig(target.URL)
	for _, op := range []string{"validate_config", "plan_run"} {
		if code, body := post(op, map[string]any{"config": cfg}); code != 200 {
			t.Fatalf("%s: %d %s", op, code, body)
		}
	}
	code, body := post("start_run", map[string]any{"config": cfg})
	if code != 200 {
		t.Fatalf("start: %d %s", code, body)
	}
	var started ops.StartOut
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	var waited ops.WaitOut
	for deadline := time.Now().Add(60 * time.Second); !waited.Finished && time.Now().Before(deadline); {
		_, body := post("wait_for_run", map[string]any{"run_id": started.RunID, "timeout_s": 10})
		if err := json.Unmarshal(body, &waited); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
	}
	if !waited.Finished || waited.ExitCode == nil || *waited.ExitCode != 0 {
		t.Fatalf("wait = %+v", waited)
	}
	if code, body := post("get_run_digest", map[string]any{"run_id": started.RunID}); code != 200 || !strings.Contains(string(body), started.RunID) {
		t.Fatalf("digest: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, started.RunID, "result.json")); err != nil {
		t.Fatal(err)
	}
}

// The same loop through `tracepoint mcp` over stdio, as a client launches it.
func TestAgentLoopThroughMCPStdio(t *testing.T) {
	target := newServer(t, 0)
	root := filepath.Join(t.TempDir(), "runs")
	ctx := context.Background()
	cmd := serverCmd(ctx, t, "mcp", "--run-root", root)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "1"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	defer func() {
		if cerr := cs.Close(); cerr != nil {
			t.Logf("close: %v", cerr)
		}
	}()

	call := func(name string, args map[string]any, into any) {
		t.Helper()
		res, cerr := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if cerr != nil {
			t.Fatalf("%s: %v\n%s", name, cerr, stderr.String())
		}
		if res.IsError {
			t.Fatalf("%s: %+v", name, res.Content[len(res.Content)-1])
		}
		if into != nil {
			b, _ := json.Marshal(res.StructuredContent)
			if uerr := json.Unmarshal(b, into); uerr != nil {
				t.Fatal(uerr)
			}
		}
	}
	cfg := agentConfig(target.URL)
	call("validate_config", map[string]any{"config": cfg}, nil)
	call("plan_run", map[string]any{"config": cfg}, nil)
	var started ops.StartOut
	call("start_run", map[string]any{"config": cfg}, &started)
	var waited ops.WaitOut
	for deadline := time.Now().Add(60 * time.Second); !waited.Finished && time.Now().Before(deadline); {
		call("wait_for_run", map[string]any{"run_id": started.RunID, "timeout_s": 10}, &waited)
	}
	if !waited.Finished || *waited.ExitCode != 0 {
		t.Fatalf("wait = %+v", waited)
	}
	var digest map[string]any
	call("get_run_digest", map[string]any{"run_id": started.RunID}, &digest)
	if digest["run_id"] != started.RunID {
		t.Fatalf("digest = %v", digest)
	}
	audit, err := os.ReadFile(filepath.Join(root, "audit.ndjson"))
	if err != nil || !strings.Contains(string(audit), `"mcp:e2e"`) {
		t.Fatalf("audit: %v\n%s", err, audit)
	}
}
