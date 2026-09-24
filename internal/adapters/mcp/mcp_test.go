package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/adapters/mcp"
	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/ops/opstest"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}

// connect starts the server over an in-memory transport and returns a client session.
func connect(t *testing.T, h *opstest.Harness) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := mcp.New(h.Service, nil).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "unit-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Wait()
	})
	return cs
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func structured(t *testing.T, res *sdk.CallToolResult, into any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
}

func text(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// The tool list is the registry, name for name and schema for schema, and it is
// recorded: a change to what an agent is shown is a reviewed change.
func TestToolsAreTheRegistry(t *testing.T) {
	cs := connect(t, opstest.New(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := ops.Registry()
	if len(res.Tools) != len(reg) {
		t.Fatalf("%d tools for %d operations", len(res.Tools), len(reg))
	}
	byName := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	for _, op := range reg {
		tool := byName[op.Name]
		if tool == nil {
			t.Errorf("no tool for %s", op.Name)
			continue
		}
		if tool.Description != op.Description || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s differs from its operation", op.Name)
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != op.Annotations.ReadOnly {
			t.Errorf("%s: annotations %+v", op.Name, tool.Annotations)
		}
	}
	b, err := json.MarshalIndent(res.Tools, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "tools.golden.json", append(b, '\n'))
}

// A whole agent loop over MCP: validate, plan, start, wait, digest.
func TestAgentLoopOverMCP(t *testing.T) {
	h := opstest.New(t)
	cs := connect(t, h)
	cfg := h.Config("2s")

	var valid ops.ValidateOut
	structured(t, callTool(t, cs, "validate_config", map[string]any{"config": cfg}), &valid)
	if !valid.Valid {
		t.Fatalf("validate = %+v", valid)
	}
	if res := callTool(t, cs, "plan_run", map[string]any{"config": cfg}); res.IsError || !strings.Contains(text(res), "plan") {
		t.Fatalf("plan: %s", text(res))
	}
	var started ops.StartOut
	structured(t, callTool(t, cs, "start_run", map[string]any{"config": cfg}), &started)

	var waited ops.WaitOut
	deadline := time.Now().Add(30 * time.Second)
	for !waited.Finished && time.Now().Before(deadline) {
		structured(t, callTool(t, cs, "wait_for_run", map[string]any{"run_id": started.RunID, "timeout_s": 5}), &waited)
	}
	if !waited.Finished || waited.ExitCode == nil || *waited.ExitCode != 0 {
		t.Fatalf("wait = %+v", waited)
	}

	res := callTool(t, cs, "get_run_digest", map[string]any{"run_id": started.RunID})
	if res.IsError {
		t.Fatal(text(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, schemas.Digest, b)

	// The same digest is a resource, and the audit log names the client.
	rr, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "tracepoint://runs/" + started.RunID + "/digest"})
	if err != nil || len(rr.Contents) != 1 || !strings.Contains(rr.Contents[0].Text, started.RunID) {
		t.Fatalf("digest resource: %v %+v", err, rr)
	}
	audit, err := os.ReadFile(h.Service.Store.AuditPath())
	if err != nil || !strings.Contains(string(audit), `"mcp:unit-test"`) {
		t.Fatalf("audit log: %v\n%s", err, audit)
	}
}

// A failed call is a tool result the model can see, carrying the coded envelope; bad
// input is refused before any handler runs.
func TestErrorsAreToolResults(t *testing.T) {
	cs := connect(t, opstest.New(t))
	res := callTool(t, cs, "get_run_status", map[string]any{"run_id": "nope"})
	if !res.IsError || !strings.Contains(text(res), `"code":"RUN_NOT_FOUND"`) {
		t.Fatalf("an unknown run: %s", text(res))
	}
	res = callTool(t, cs, "get_run_status", map[string]any{"run_id": "x", "surprise": true})
	if !res.IsError || !strings.Contains(text(res), "OPS_INVALID_INPUT") {
		t.Fatalf("an unknown field: %s", text(res))
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	cs := connect(t, opstest.New(t))
	ctx := context.Background()

	list, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	uris := map[string]bool{}
	for _, r := range list.Resources {
		uris[r.URI] = true
	}
	for _, name := range schemas.Names() {
		if !uris["tracepoint://schemas/"+string(name)] {
			t.Errorf("no resource for the %s schema", name)
		}
	}
	for _, uri := range []string{"tracepoint://docs/methodology", "tracepoint://docs/errors"} {
		doc, rerr := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
		if rerr != nil || len(doc.Contents) != 1 || len(doc.Contents[0].Text) < 500 {
			t.Errorf("%s: %v", uri, rerr)
		}
	}
	rr, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "tracepoint://schemas/digest"})
	if err != nil || !json.Valid([]byte(rr.Contents[0].Text)) {
		t.Fatalf("the digest schema: %v", err)
	}
	if _, rerr := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "tracepoint://runs/nope/digest"}); rerr == nil {
		t.Fatal("an unknown run's digest was served")
	}

	tmpl, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(tmpl.ResourceTemplates) != 2 {
		t.Fatalf("templates: %v %+v", err, tmpl)
	}

	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 3 {
		t.Fatalf("prompts: %v %+v", err, prompts)
	}
	got, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "hunt_bottleneck", Arguments: map[string]string{"target": "http://127.0.0.1:8080", "slo": "p99 250ms"}})
	if err != nil || len(got.Messages) != 1 {
		t.Fatalf("hunt_bottleneck: %v", err)
	}
	if body := got.Messages[0].Content.(*sdk.TextContent).Text; !strings.Contains(body, "http://127.0.0.1:8080") || !strings.Contains(body, "p99 250ms") || !strings.Contains(body, "get_policy") {
		t.Fatalf("prompt body: %s", body)
	}
	if _, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "hunt_bottleneck"}); err == nil {
		t.Fatal("a required argument was not enforced")
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\nrun `go test ./internal/adapters/mcp/ -update` to create it", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed; review the difference and rerun with -update if it is intended\n--- got ---\n%s", path, got)
	}
}
