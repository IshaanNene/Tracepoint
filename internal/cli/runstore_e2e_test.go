package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/cli"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
)

var (
	binaryOnce sync.Once
	binaryDir  string
	binaryPath string
	binaryErr  error
)

// binary builds the real tracepoint once: a detached run re-executes the binary, and
// the test binary is not it.
func binary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		binaryDir, binaryErr = os.MkdirTemp("", "tracepoint-cli-test-")
		if binaryErr != nil {
			return
		}
		binaryPath = filepath.Join(binaryDir, "tracepoint")
		out, err := osexec.CommandContext(context.Background(), "go", "build", "-o", binaryPath, "../../cmd/tracepoint").CombinedOutput()
		if err != nil {
			binaryErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

func cleanupBinary() {
	if binaryDir != "" {
		_ = os.RemoveAll(binaryDir)
	}
}

// execWith runs the command tree with an explicit run root and executable.
func execWith(t *testing.T, root, executable string, args ...string) run {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := cli.Execute(context.Background(), cli.Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf, Args: args,
		Lookup:  func(string) (string, bool) { return "", false },
		RunRoot: root, Executable: executable, Actor: "test",
	})
	return run{code: code, stdout: out.String(), stderr: errBuf.String()}
}

func simpleConfig(t *testing.T, url, duration string) string {
	t.Helper()
	return writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: %s, bucket: 500ms, seed: 5, timeout: 2s }
http:
  base_url: %q
  headers: { Authorization: "Bearer inline-secret" }
  executor: { rate: 20, max_in_flight: 8 }
  requests: [{ name: items, url: "/api/items" }]
`, duration, url))
}

func onlyRun(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) != 1 {
		t.Fatalf("runs under %s: %v, want one", root, ids)
	}
	return ids[0]
}

// Every run leaves a complete directory (§6.4), every event validates, the stored
// configuration holds no secret, and the audit log records it.
func TestRunWritesItsDirectory(t *testing.T) {
	srv := newServer(t, 0)
	root := filepath.Join(t.TempDir(), "runs")
	r := execWith(t, root, "", "run", "-c", simpleConfig(t, srv.URL, "2s"), "--log-level", "error")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	id := onlyRun(t, root)
	dir := filepath.Join(root, id)
	for _, f := range []string{"state.json", "events.ndjson", "result.json", "digest.json", "config.effective.yaml", "run.log"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}

	var st runstore.State
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != runstore.StatusCompleted || st.ExitCode == nil || *st.ExitCode != 0 || st.Actor != "test" {
		t.Fatalf("state = %+v", st)
	}

	events := compileSchema(t, "events")
	f, err := os.Open(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var types []string
	seq := 0.0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		ev := validateAgainst(t, events, sc.Text(), "event")
		types = append(types, ev["type"].(string))
		if s := ev["seq"].(float64); s != seq+1 {
			t.Fatalf("seq %v after %v", s, seq)
		} else {
			seq = s
		}
	}
	if types[0] != "preflight.completed" || types[1] != "run.started" || types[len(types)-1] != "run.finished" {
		t.Fatalf("event types = %v", types)
	}
	if !contains(types, "bucket.sealed") {
		t.Fatalf("no bucket.sealed in %v", types)
	}

	for _, name := range []string{"state.json", "events.ndjson", "result.json", "digest.json", "config.effective.yaml", "run.log"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if bytes.Contains(b, []byte("inline-secret")) {
			t.Errorf("the inline secret leaked into %s", name)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(root, "audit.ndjson"))
	if n := bytes.Count(audit, []byte("\n")); n != 2 || !bytes.Contains(audit, []byte(`"event":"finished"`)) {
		t.Fatalf("audit log:\n%s", audit)
	}

	// digest accepts the run id now that runs have a store.
	d := execWith(t, root, "", "digest", id)
	validateAgainst(t, compileSchema(t, "digest"), d.stdout, "digest")
}

// With --events -, stdout is the event stream and nothing else, ending in run.finished
// with the digest and the exit code.
func TestEventsOnStdout(t *testing.T) {
	srv := newServer(t, 0)
	r := execWith(t, filepath.Join(t.TempDir(), "runs"), "", "run", "-c", simpleConfig(t, srv.URL, "1s"),
		"--events", "-", "--log-level", "error")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	schema := compileSchema(t, "events")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	var last map[string]any
	for _, l := range lines {
		last = validateAgainst(t, schema, l, "event")
	}
	data, _ := last["data"].(map[string]any)
	if last["type"] != "run.finished" || data["exit_code"] != 0.0 {
		t.Fatalf("last event = %v", last)
	}
	digest, _ := json.Marshal(data["digest"])
	validateAgainst(t, compileSchema(t, "digest"), string(digest), "embedded digest")
}

// A second run is refused while one is active: two tests against one target measure
// each other.
func TestConcurrentRunsAreRefused(t *testing.T) {
	srv := newServer(t, 0)
	root := filepath.Join(t.TempDir(), "runs")
	store, err := runstore.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := store.Create("20260924T000000Z-busy00")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = busy.UpdateState(func(st *runstore.State) { st.Status, st.Heartbeat = runstore.StatusRunning, now })

	r := execWith(t, root, "", "run", "-c", simpleConfig(t, srv.URL, "1s"), "--output", "json")
	if r.code != errs.ExitUsage || !strings.Contains(r.stdout, "POLICY_TOO_MANY_RUNS") {
		t.Fatalf("exit %d: %s", r.code, r.stdout)
	}
}

// --from re-runs a stored configuration; a secret it had to redact is refused until
// supplied again.
func TestFromNeedsRedactedSecretsAgain(t *testing.T) {
	srv := newServer(t, 0)
	root := filepath.Join(t.TempDir(), "runs")
	if r := execWith(t, root, "", "run", "-c", simpleConfig(t, srv.URL, "1s"), "--log-level", "error"); r.code != 0 {
		t.Fatalf("first run: %d %s", r.code, r.stderr)
	}
	id := onlyRun(t, root)

	r := execWith(t, root, "", "run", "--from", id, "--output", "json")
	if r.code != errs.ExitUsage || !strings.Contains(r.stdout, "http.headers.Authorization") {
		t.Fatalf("exit %d: %s", r.code, r.stdout)
	}
	r = execWith(t, root, "", "run", "--from", id, "--set", "http.headers.Authorization=Bearer again",
		"--set", "run.duration=1s", "--output", "json", "--log-level", "error")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result")
	cfgDoc, _ := doc["config"].(map[string]any)
	if overrides, _ := cfgDoc["overrides"].([]any); len(overrides) != 2 {
		t.Fatalf("overrides = %v", cfgDoc["overrides"])
	}
}

func TestListStatusAndGC(t *testing.T) {
	srv := newServer(t, 0)
	root := filepath.Join(t.TempDir(), "runs")
	for range 3 {
		if r := execWith(t, root, "", "run", "-c", simpleConfig(t, srv.URL, "500ms"), "--log-level", "error"); r.code != 0 {
			t.Fatalf("run: %d %s", r.code, r.stderr)
		}
		time.Sleep(1100 * time.Millisecond) // run ids carry the second
	}
	var list struct {
		Runs []runstore.State `json:"runs"`
	}
	r := execWith(t, root, "", "list", "--output", "json")
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list.Runs) != 3 {
		t.Fatalf("list: %v %s", err, r.stdout)
	}
	newest := list.Runs[0].RunID
	if s := execWith(t, root, "", "status", newest[len(newest)-6:], "--output", "json"); !strings.Contains(s.stdout, `"completed"`) {
		t.Fatalf("status by suffix: %s %s", s.stdout, s.stderr)
	}
	if s := execWith(t, root, "", "status", "nope", "--output", "json"); s.code != errs.ExitUsage || !strings.Contains(s.stdout, "RUN_NOT_FOUND") {
		t.Fatalf("status of nothing: %d %s", s.code, s.stdout)
	}
	g := execWith(t, root, "", "gc", "--keep", "1", "--output", "json")
	if !strings.Contains(g.stdout, `"removed"`) || strings.Contains(g.stdout, newest) {
		t.Fatalf("gc: %s", g.stdout)
	}
	r = execWith(t, root, "", "list", "--output", "json")
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list.Runs) != 1 || list.Runs[0].RunID != newest {
		t.Fatalf("after gc: %s", r.stdout)
	}
}

// The detached lifecycle with the real binary: return at once, wait with a timeout
// (exit 5 while running), stop gracefully, and wait for the interrupted result.
func TestDetachWaitStop(t *testing.T) {
	exe := binary(t)
	srv := newServer(t, 0)
	root := filepath.Join(t.TempDir(), "runs")

	start := time.Now()
	r := execWith(t, root, exe, "run", "-c", simpleConfig(t, srv.URL, "30s"), "--detach", "--output", "json")
	if r.code != errs.ExitOK {
		t.Fatalf("detach: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("--detach took %s: it must return once preflight passes", time.Since(start))
	}
	var started struct {
		RunID  string `json:"run_id"`
		RunDir string `json:"run_dir"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &started); err != nil || started.RunID == "" {
		t.Fatalf("detach output: %v %s", err, r.stdout)
	}

	w := execWith(t, root, exe, "wait", started.RunID, "--timeout", "2s", "--output", "json")
	if w.code != errs.ExitWaitOpen || !strings.Contains(w.stdout, `"running"`) {
		t.Fatalf("wait while running: exit %d %s %s", w.code, w.stdout, w.stderr)
	}

	if s := execWith(t, root, exe, "stop", started.RunID, "--output", "json"); s.code != 0 {
		t.Fatalf("stop: %d %s", s.code, s.stdout)
	}
	w = execWith(t, root, exe, "wait", started.RunID, "--timeout", "20s", "--output", "json")
	if w.code != errs.ExitRuntime {
		t.Fatalf("wait after stop: exit %d %s", w.code, w.stdout)
	}
	var st runstore.State
	if err := json.Unmarshal([]byte(w.stdout), &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != runstore.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", st.Status)
	}
	res, err := os.ReadFile(filepath.Join(started.RunDir, "result.json"))
	if err != nil {
		t.Fatalf("no partial result: %v", err)
	}
	validateAgainst(t, compileSchema(t, "result"), string(res), "result")
	if !bytes.Contains(res, []byte("stop requested")) {
		t.Fatalf("the result does not say why it stopped")
	}
}

// A detached run whose preflight fails is reported synchronously and never starts.
func TestDetachReportsPreflightSynchronously(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	r := execWith(t, root, binary(t), "run", "-c", simpleConfig(t, "http://127.0.0.1:1", "5s"), "--detach", "--output", "json")
	if r.code != errs.ExitRuntime || !strings.Contains(r.stdout, "PREFLIGHT_") {
		t.Fatalf("exit %d: %s", r.code, r.stdout)
	}
	st := execWith(t, root, "", "list", "--output", "json")
	if !strings.Contains(st.stdout, `"failed"`) {
		t.Fatalf("the refused run should be recorded as failed: %s", st.stdout)
	}
}
