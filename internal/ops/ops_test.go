package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
	"github.com/IshaanNene/Tracepoint/internal/session"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}

// harness is a service whose runs execute in this process, so tests are fast and
// deterministic; the detached-process launcher is exercised by the CLI suite.
type harness struct {
	svc *ops.Service
	srv *httptest.Server
	dir string
	wg  sync.WaitGroup
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{dir: t.TempDir()}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	store, err := runstore.Open(filepath.Join(h.dir, "runs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = &ops.Service{
		Policy: policy.ServerDefault(), Store: store, Root: h.dir, Actor: "test",
		Lookup: func(string) (string, bool) { return "", false },
		Launch: func(_ context.Context, p *session.Prepared) (int, error) {
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				_, _ = p.Execute(context.Background())
			}()
			return os.Getpid(), nil
		},
	}
	t.Cleanup(func() {
		h.wg.Wait()
		h.srv.Close()
	})
	return h
}

func (h *harness) config(duration string) string {
	return fmt.Sprintf(`
version: 1
run: { duration: %s, bucket: 500ms, seed: 3, timeout: 2s }
slo: { http: { p99: 2s } }
http:
  base_url: %q
  executor: { rate: 20, max_in_flight: 8 }
  requests: [{ name: items, url: "/api/items" }]
`, duration, h.srv.URL)
}

func call(t *testing.T, h *harness, name string, in any) (any, error) {
	t.Helper()
	op, err := ops.Find(name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return op.Invoke(context.Background(), h.svc, raw)
}

func code(err error) errs.Code {
	var typed *errs.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

func TestRegistryIsComplete(t *testing.T) {
	var names []string
	for _, op := range ops.Registry() {
		names = append(names, op.Name)
		if op.Description == "" || op.Title == "" || op.Input == nil || op.Output == nil {
			t.Errorf("%s is incompletely defined", op.Name)
		}
		if len(op.Description) < 120 {
			t.Errorf("%s: the description must say what it does and when to use it", op.Name)
		}
	}
	want := "get_policy get_run_digest get_run_section get_run_status list_runs plan_run render_report scaffold_config start_run stop_run validate_config wait_for_run"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("operations = %s", got)
	}
	if _, err := ops.Find("launch_missiles"); code(err) != errs.CodeOpsUnknownOperation {
		t.Fatalf("an unknown operation: %v", err)
	}
}

// Input is validated against the schema the agent was shown, and unknown fields are
// refused rather than silently ignored.
func TestInputValidation(t *testing.T) {
	h := newHarness(t)
	for name, in := range map[string]any{
		"wait_for_run":    map[string]any{"run_id": "x", "timeout_s": 500},
		"get_run_status":  map[string]any{"run_idd": "x"},
		"get_run_section": map[string]any{"run_id": "x", "section": "secrets"},
		"validate_config": map[string]any{"config": 7},
	} {
		if _, err := call(t, h, name, in); code(err) != errs.CodeOpsInvalidInput {
			t.Errorf("%s(%v): %v, want OPS_INVALID_INPUT", name, in, err)
		}
	}
	op, _ := ops.Find("get_policy")
	if _, err := op.Invoke(context.Background(), h.svc, json.RawMessage("not json")); code(err) != errs.CodeOpsInvalidInput {
		t.Fatalf("malformed JSON: %v", err)
	}
	if _, err := op.Invoke(context.Background(), h.svc, nil); err != nil {
		t.Fatalf("no input at all is an empty object: %v", err)
	}
}

func TestPolicyIsTheServers(t *testing.T) {
	h := newHarness(t)
	out, err := call(t, h, "get_policy", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	p := out.(*ops.PolicyOut)
	if p.Policy.AllowWrites || p.Policy.MaxRatePerRunner == nil || *p.Policy.MaxRatePerRunner != 500 {
		t.Fatalf("policy = %+v", p.Policy)
	}
}

// A configuration cannot exceed the server's policy: the refusal names what a human
// would have to change.
func TestPolicyCannotBeExceeded(t *testing.T) {
	h := newHarness(t)
	cfg := strings.Replace(h.config("5s"), "rate: 20", "rate: 5000", 1)
	_, err := call(t, h, "validate_config", map[string]any{"config": cfg})
	if code(err) != errs.CodePolicyRateExceeded {
		t.Fatalf("5000 req/s under a 500 req/s policy: %v", err)
	}
}

func TestConfigSources(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.dir, "tp.yaml"), []byte(h.config("5s")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, h, "validate_config", map[string]any{"config_path": "tp.yaml"}); err != nil {
		t.Fatalf("a path inside the root: %v", err)
	}
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd"} {
		if _, err := call(t, h, "validate_config", map[string]any{"config_path": bad}); code(err) != errs.CodeOpsInvalidInput {
			t.Errorf("%s escaped the root: %v", bad, err)
		}
	}
	if err := os.Symlink("/etc", filepath.Join(h.dir, "escape")); err == nil {
		if _, err := call(t, h, "validate_config", map[string]any{"config_path": "escape/hostname"}); code(err) != errs.CodeOpsInvalidInput {
			t.Errorf("a symbolic link escaped the root: %v", err)
		}
	}
	// JSON is YAML: an object is accepted as a document.
	obj := map[string]any{"version": 1, "run": map[string]any{"duration": "5s"},
		"http": map[string]any{"executor": map[string]any{"rate": 5}, "requests": []any{map[string]any{"name": "a", "url": h.srv.URL}}}}
	out, err := call(t, h, "validate_config", map[string]any{"config": obj, "overrides": []string{"run.duration=7s"}})
	if err != nil {
		t.Fatalf("an object configuration: %v", err)
	}
	if v := out.(*ops.ValidateOut); !v.Valid || v.Duration != "7s" {
		t.Fatalf("validate = %+v", v)
	}
	if _, err := call(t, h, "validate_config", map[string]any{"config": "x", "config_path": "tp.yaml"}); code(err) != errs.CodeOpsInvalidInput {
		t.Fatalf("both sources: %v", err)
	}
}

func TestScaffoldValidates(t *testing.T) {
	h := newHarness(t)
	out, err := call(t, h, "scaffold_config", map[string]any{
		"base_url": h.srv.URL, "paths": []string{"/api/items", "/api/users/me"},
		"db_driver": "postgres", "db_dsn_env": "PG_DSN", "redis_addr_env": "REDIS_ADDR", "rate": 15,
	})
	if err != nil {
		t.Fatal(err)
	}
	yaml := out.(*ops.ScaffoldOut).ConfigYAML
	for _, want := range []string{"${PG_DSN}", "${REDIS_ADDR}", "api-items", "api-users-me", "telemetry: { postgres: true, redis: true }"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("starter lacks %q:\n%s", want, yaml)
		}
	}
	h.svc.Lookup = func(k string) (string, bool) {
		v, ok := map[string]string{"PG_DSN": "postgres://u@127.0.0.1/app", "REDIS_ADDR": "127.0.0.1:6379"}[k]
		return v, ok
	}
	if _, err := call(t, h, "validate_config", map[string]any{"config": yaml}); err != nil {
		t.Fatalf("the starter does not validate: %v\n%s", err, yaml)
	}
	if _, err := call(t, h, "scaffold_config", map[string]any{"base_url": "x", "db_dsn_env": "postgres://u:p@h/db"}); code(err) != errs.CodeOpsInvalidInput {
		t.Fatalf("a DSN value where a name belongs: %v", err)
	}
}

// The whole agent loop through the registry: plan, start, wait, digest, sections,
// list, and stop on a finished run.
func TestAgentLoop(t *testing.T) {
	h := newHarness(t)
	cfg := h.config("2s")

	planOut, err := call(t, h, "plan_run", map[string]any{"config": cfg})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(planOut); !strings.Contains(string(b), `"peak_rps":20`) {
		t.Fatalf("plan = %s", b)
	}

	started, err := call(t, h, "start_run", map[string]any{"config": cfg})
	if err != nil {
		t.Fatal(err)
	}
	id := started.(*ops.StartOut).RunID

	// A second run is refused while the first is active.
	if _, err = call(t, h, "start_run", map[string]any{"config": cfg}); code(err) != errs.CodePolicyTooManyRuns {
		t.Fatalf("a concurrent run: %v", err)
	}

	var waited *ops.WaitOut
	for range 10 {
		out, werr := call(t, h, "wait_for_run", map[string]any{"run_id": id, "timeout_s": 5})
		if werr != nil {
			t.Fatal(werr)
		}
		if waited = out.(*ops.WaitOut); waited.Finished {
			break
		}
	}
	if !waited.Finished || waited.Digest == nil || *waited.ExitCode != 0 {
		t.Fatalf("wait = %+v", waited)
	}

	d, err := call(t, h, "get_run_digest", map[string]any{"run_id": id, "budget_chars": 1500})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	schematest.Validate(t, schemas.Digest, raw)
	if dig := d.(*result.Digest); dig.RunID != id {
		t.Fatalf("digest of %s", dig.RunID)
	}

	sec, err := call(t, h, "get_run_section", map[string]any{"run_id": id, "section": "timeline", "page_size": 2})
	if err != nil {
		t.Fatal(err)
	}
	s := sec.(*ops.SectionOut)
	if s.Total < 3 || len(s.Items) != 2 || s.Pages < 2 {
		t.Fatalf("timeline = %+v", s)
	}
	if sec, serr := call(t, h, "get_run_section", map[string]any{"run_id": id, "section": "telemetry", "runner": "generator"}); serr != nil || sec.(*ops.SectionOut).Total == 0 {
		t.Fatalf("telemetry: %v %+v", serr, sec)
	}
	if _, err = call(t, h, "get_run_section", map[string]any{"run_id": id, "section": "timeline", "runner": "nope"}); code(err) != errs.CodeOpsInvalidInput {
		t.Fatalf("an unknown runner: %v", err)
	}

	rep, err := call(t, h, "render_report", map[string]any{"run_id": id})
	if ro, ok := rep.(*ops.ReportOut); err != nil || !ok || ro.Format != "markdown" || !strings.HasPrefix(ro.Content, "### TracePoint: ") {
		t.Fatalf("render_report markdown: %v %+v", err, rep)
	}
	rep, err = call(t, h, "render_report", map[string]any{"run_id": id, "format": "html"})
	if ro, ok := rep.(*ops.ReportOut); err != nil || !ok || ro.Content != "" || ro.Bytes < 50_000 {
		t.Fatalf("render_report html returns a path, not the page: %v %+v", err, rep)
	}
	if _, err = call(t, h, "render_report", map[string]any{"run_id": id, "format": "pdf"}); code(err) != errs.CodeOpsInvalidInput {
		t.Fatalf("an unknown format: %v", err)
	}

	list, err := call(t, h, "list_runs", map[string]any{})
	if err != nil || len(list.(*ops.ListOut).Runs) != 1 {
		t.Fatalf("list = %+v %v", list, err)
	}
	st, err := call(t, h, "get_run_status", map[string]any{"run_id": id})
	if err != nil || st.(*runstore.State).Status != runstore.StatusCompleted {
		t.Fatalf("status = %+v %v", st, err)
	}
	stop, err := call(t, h, "stop_run", map[string]any{"run_id": id})
	if err != nil || stop.(*ops.StopOut).Stopped {
		t.Fatalf("stopping a finished run: %+v %v", stop, err)
	}
	if _, err := call(t, h, "get_run_digest", map[string]any{"run_id": "nope"}); code(err) != errs.CodeRunNotFound {
		t.Fatalf("an unknown run: %v", err)
	}
}

// A run stopped through the registry ends interrupted with its partial result.
func TestStopRun(t *testing.T) {
	h := newHarness(t)
	started, err := call(t, h, "start_run", map[string]any{"config": h.config("30s")})
	if err != nil {
		t.Fatal(err)
	}
	id := started.(*ops.StartOut).RunID
	out, err := call(t, h, "wait_for_run", map[string]any{"run_id": id, "timeout_s": 1})
	if err != nil || out.(*ops.WaitOut).Finished {
		t.Fatalf("a 30s run finished within a second: %+v %v", out, err)
	}
	if s, serr := call(t, h, "stop_run", map[string]any{"run_id": id}); serr != nil || !s.(*ops.StopOut).Stopped {
		t.Fatalf("stop = %+v %v", s, serr)
	}
	out, err = call(t, h, "wait_for_run", map[string]any{"run_id": id, "timeout_s": 20})
	if err != nil {
		t.Fatal(err)
	}
	w := out.(*ops.WaitOut)
	if !w.Finished || w.State.Status != runstore.StatusInterrupted || w.Digest == nil {
		t.Fatalf("after stop: %+v", w)
	}
}

func TestSummaries(t *testing.T) {
	op, _ := ops.Find("get_run_status")
	if s := ops.Summary(op, nil, errs.New(errs.CodeRunNotFound, "no run").WithHint("list them")); !strings.Contains(s, "RUN_NOT_FOUND") || !strings.Contains(s, "list them") {
		t.Fatalf("summary = %q", s)
	}
	w, _ := ops.Find("wait_for_run")
	if s := ops.Summary(w, &ops.WaitOut{State: runstore.State{RunID: "r", Status: "running"}}, nil); !strings.Contains(s, "call wait_for_run again") {
		t.Fatalf("summary = %q", s)
	}
}

// scaffold_config reads a project directory and an OpenAPI document only inside the
// server's directory, and what it produces validates.
func TestScaffoldDetects(t *testing.T) {
	h := newHarness(t)
	if err := os.CopyFS(filepath.Join(h.dir, "shop"), os.DirFS("../detect/testdata/shop")); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, h, "scaffold_config", map[string]any{"detect_dir": "shop"})
	if err != nil {
		t.Fatal(err)
	}
	sc := out.(*ops.ScaffoldOut)
	if len(sc.Inferences) == 0 {
		t.Fatal("no inferences")
	}
	for _, want := range []string{"driver: postgres", "${DATABASE_URL}", "${REDIS_ADDR}", "# TODO:", "http://127.0.0.1:8080"} {
		if !strings.Contains(sc.ConfigYAML, want) {
			t.Errorf("starter lacks %q:\n%s", want, sc.ConfigYAML)
		}
	}
	h.svc.Lookup = func(k string) (string, bool) {
		v, ok := map[string]string{"DATABASE_URL": "postgres://u@127.0.0.1/app", "REDIS_ADDR": "127.0.0.1:6379"}[k]
		return v, ok
	}
	if _, err = call(t, h, "validate_config", map[string]any{"config": sc.ConfigYAML}); err != nil {
		t.Fatalf("the detected starter does not validate: %v\n%s", err, sc.ConfigYAML)
	}

	out, err = call(t, h, "scaffold_config", map[string]any{"openapi_path": "shop/api/openapi.yaml", "base_url": h.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if y := out.(*ops.ScaffoldOut).ConfigYAML; !strings.Contains(y, "method: GET") || strings.Contains(y, "method: POST") {
		t.Fatalf("imported starter:\n%s", y)
	}
	if _, err := call(t, h, "scaffold_config", map[string]any{"openapi_path": "shop/api/openapi.yaml"}); code(err) != errs.CodeOpsInvalidInput {
		t.Fatalf("an import without base_url: %v", err)
	}
	for _, in := range []map[string]any{
		{"detect_dir": "../"},
		{"openapi_path": "../../etc/passwd", "base_url": h.srv.URL},
	} {
		if _, err := call(t, h, "scaffold_config", in); err == nil {
			t.Fatalf("%v escaped the server's directory", in)
		}
	}
}
