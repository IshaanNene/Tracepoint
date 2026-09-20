package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/cli"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		// signal.NotifyContext leaves a watcher for the life of the process.
		goleak.IgnoreAnyFunction("os/signal.loop"),
		goleak.IgnoreAnyFunction("os/signal.signalWaitUntilIdle"),
	)
}

type run struct {
	code   int
	stdout string
	stderr string
}

// exec runs the command tree exactly as main does, with streams captured.
func exec(t *testing.T, args ...string) run {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := cli.Execute(context.Background(), cli.Env{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errBuf,
		Args:   args,
		Lookup: func(string) (string, bool) { return "", false },
		IsTTY:  false,
	})
	return run{code: code, stdout: out.String(), stderr: errBuf.String()}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tracepoint.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
	return path
}

// fastServer answers immediately; slowServer takes the given time.
func newServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		fmt.Fprint(w, `{"items":[1,2,3]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// compileSchema loads one of the embedded contracts so emitted documents can be
// checked against the very schema the binary ships.
func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	r := exec(t, "schema", name)
	if r.code != 0 {
		t.Fatalf("schema %s exited %d: %s", name, r.code, r.stderr)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(r.stdout))
	if err != nil {
		t.Fatalf("the emitted %s schema is not valid JSON: %v", name, err)
	}
	c := jsonschema.NewCompiler()
	url := "mem://" + name + ".json"
	if addErr := c.AddResource(url, doc); addErr != nil {
		t.Fatalf("adding the %s schema: %v", name, addErr)
	}
	compiled, compileErr := c.Compile(url)
	if compileErr != nil {
		t.Fatalf("compiling the %s schema: %v", name, compileErr)
	}
	return compiled
}

// validateAgainst checks a document against a schema and returns it decoded for
// assertions.
//
// The validation and the decoding are deliberately separate passes. The schema
// library parses with UseNumber, so every number comes back as a json.Number; asserting
// `.(float64)` against that silently yields zero rather than failing, which turns a
// broken assertion into a passing-looking test. encoding/json gives float64, which is
// what the assertions below expect.
func validateAgainst(t *testing.T, s *jsonschema.Schema, raw, what string) map[string]any {
	t.Helper()
	forSchema, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", what, err, truncate(raw))
	}
	if err := s.Validate(forSchema); err != nil {
		t.Fatalf("%s does not satisfy its own schema:\n%v", what, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s could not be decoded for assertions: %v", what, err)
	}
	return out
}

// number pulls a numeric field out, failing loudly rather than defaulting to zero when
// the path is wrong - the failure mode that made this harness lie in the first place.
func number(t *testing.T, m map[string]any, path ...string) float64 {
	t.Helper()
	cur := any(m)
	for i, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", strings.Join(path[:i], "."))
		}
		cur, ok = obj[key]
		if !ok {
			t.Fatalf("no field %s", strings.Join(path[:i+1], "."))
		}
	}
	v, ok := cur.(float64)
	if !ok {
		t.Fatalf("%s is %T, not a number", strings.Join(path, "."), cur)
	}
	return v
}

func truncate(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

// A clean run against a healthy target: exit 0, and a result that satisfies the
// schema this binary ships.
func TestCleanRunExitsZeroAndEmitsASchemaValidResult(t *testing.T) {
	srv := newServer(t, 0)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 500ms, seed: 42, timeout: 3s }
slo:
  http: { p99: 2s, error_rate: 0.05 }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 40, max_in_flight: 16 }
  requests:
    - { name: list-items, url: "/api/items" }
`, srv.URL))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s\nstdout: %s", r.code, r.stderr, truncate(r.stdout))
	}

	doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result.json")

	analysis, _ := doc["analysis"].(map[string]any)
	validity, _ := analysis["validity"].(map[string]any)
	// Loopback makes it degraded, which is correct and is the point of the caveat.
	if state := validity["state"]; state != "degraded" && state != "valid" {
		t.Errorf("validity = %v, want valid or degraded", state)
	}
	slo, _ := analysis["slo"].(map[string]any)
	if slo["pass"] != true {
		t.Errorf("slo.pass = %v, want true", slo["pass"])
	}

	runners, _ := doc["runners"].([]any)
	if len(runners) != 1 {
		t.Fatalf("got %d runners, want 1", len(runners))
	}
	rn, _ := runners[0].(map[string]any)
	if n := number(t, rn, "summary", "n"); n < 20 {
		t.Errorf("only %v operations completed in a 2s run at 40/s", n)
	}
	if ratio := number(t, rn, "summary", "error_ratio"); ratio != 0 {
		t.Errorf("error ratio = %v against a healthy target", ratio)
	}
}

// The §6.1 contract: with --output json, stdout carries exactly one JSON document.
// An SLO breach is a field inside the result, not a second document beside it.
func TestJSONOutputIsExactlyOneDocumentEvenOnABreach(t *testing.T) {
	srv := newServer(t, 60*time.Millisecond)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 500ms, seed: 42, timeout: 3s }
slo:
  http: { p99: 5ms }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 20, max_in_flight: 16 }
  requests:
    - { name: slow, url: "/api/items" }
`, srv.URL))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
	if r.code != errs.ExitBreach {
		t.Fatalf("exit %d, want %d for an SLO breach\nstderr: %s", r.code, errs.ExitBreach, r.stderr)
	}

	// Decoding with a stream decoder is what catches a second document; a plain
	// Unmarshal of the first object would happily ignore trailing content.
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("stdout is not a JSON document: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		t.Fatalf("stdout carried a second JSON document, which breaks every parser reading the stream:\n%#v", extra)
	}

	validateAgainst(t, compileSchema(t, "result"), r.stdout, "result.json")
	analysis, _ := first["analysis"].(map[string]any)
	slo, _ := analysis["slo"].(map[string]any)
	if slo["pass"] != false {
		t.Errorf("slo.pass = %v, want false", slo["pass"])
	}
}

// A run the generator bottlenecked says nothing about the target, so it exits 4 and
// says so rather than reporting a latency that is really our own queueing.
func TestGeneratorLimitedRunIsInvalid(t *testing.T) {
	srv := newServer(t, 40*time.Millisecond)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 500ms, seed: 42, timeout: 3s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 300, max_in_flight: 1, queue_depth: 0 }
  requests:
    - { name: probe, url: "/api/items" }
`, srv.URL))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
	if r.code != errs.ExitInvalid {
		t.Fatalf("exit %d, want %d for a generator-limited run\nstderr: %s", r.code, errs.ExitInvalid, r.stderr)
	}

	doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result.json")
	analysis, _ := doc["analysis"].(map[string]any)
	validity, _ := analysis["validity"].(map[string]any)
	if validity["state"] != "invalid" {
		t.Fatalf("validity = %v, want invalid", validity["state"])
	}

	findings, _ := validity["findings"].([]any)
	var codes []string
	for _, f := range findings {
		m, _ := f.(map[string]any)
		codes = append(codes, fmt.Sprint(m["code"]))
	}
	if !contains(codes, "CLIENT_CAPPED") && !contains(codes, "GENERATOR_BEHIND") {
		t.Errorf("findings = %v, want the run blamed on the generator", codes)
	}

	// The verdict must blame us, not the target, and must say the numbers cannot be read.
	verdict, _ := analysis["verdict"].(map[string]any)
	if verdict["bottleneck"] != "client" {
		t.Errorf("bottleneck = %v, want client; the target must not be blamed for our own ceiling", verdict["bottleneck"])
	}
}

func TestAllowInvalidDowngradesTheExitCode(t *testing.T) {
	srv := newServer(t, 40*time.Millisecond)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 500ms, seed: 42, timeout: 3s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 300, max_in_flight: 1, queue_depth: 0 }
  requests:
    - { name: probe, url: "/api/items" }
`, srv.URL))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error", "--allow-invalid")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d with --allow-invalid, want 0", r.code)
	}
	// The finding must survive: the flag changes the exit code, not the truth.
	doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result.json")
	analysis, _ := doc["analysis"].(map[string]any)
	validity, _ := analysis["validity"].(map[string]any)
	if validity["state"] != "invalid" {
		t.Errorf("--allow-invalid changed the verdict to %v; it must only change the exit code", validity["state"])
	}
}

// A configuration problem is reported before anything is contacted, as an error
// envelope on stdout that satisfies the error schema.
func TestConfigErrorEmitsAnErrorEnvelope(t *testing.T) {
	cfg := writeConfig(t, `
version: 1
run: { duration: 2s }
http:
  executor: { rate: 5 }
  requests:
    - name: probe
      metod: GET
      url: "http://127.0.0.1:1/"
`)
	r := exec(t, "run", "-c", cfg, "--output", "json")
	if r.code != errs.ExitUsage {
		t.Fatalf("exit %d, want %d for a configuration error", r.code, errs.ExitUsage)
	}

	doc := validateAgainst(t, compileSchema(t, "error"), r.stdout, "the error envelope")
	e, _ := doc["error"].(map[string]any)
	if e["code"] != "CONFIG_UNKNOWN_FIELD" {
		t.Errorf("code = %v, want CONFIG_UNKNOWN_FIELD", e["code"])
	}
	if hint, _ := e["hint"].(string); !strings.Contains(hint, "method") {
		t.Errorf("hint = %q, want a suggestion of \"method\"", hint)
	}
	// The document starts with a newline, so `metod` is on line 8.
	if line := number(t, doc, "error", "line"); line != 8 {
		t.Errorf("line = %v, want 8", line)
	}
	if col := number(t, doc, "error", "column"); col == 0 {
		t.Error("an unknown field should carry a column")
	}
	if e["retriable"] != false {
		t.Errorf("retriable = %v; a configuration error cannot be fixed by retrying", e["retriable"])
	}
}

// Preflight fails before any load is generated, which is what makes a wrong port cost
// a second rather than a whole run.
func TestPreflightFailureExitsThree(t *testing.T) {
	srv := newServer(t, 0)
	dead := srv.URL
	srv.Close()

	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 30s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 5, max_in_flight: 4 }
  requests:
    - { name: probe, url: "/api/items" }
`, dead))

	start := time.Now()
	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
	if r.code != errs.ExitRuntime {
		t.Fatalf("exit %d, want %d for a preflight failure", r.code, errs.ExitRuntime)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("preflight took %v; it must fail before the run's 30s duration", elapsed)
	}
	validateAgainst(t, compileSchema(t, "error"), r.stdout, "the error envelope")
}

func TestValidate(t *testing.T) {
	t.Run("accepts a good configuration", func(t *testing.T) {
		cfg := writeConfig(t, `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 5 }
  requests:
    - { name: probe, url: "http://127.0.0.1:8080/" }
`)
		r := exec(t, "validate", "-c", cfg)
		if r.code != errs.ExitOK {
			t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "valid") {
			t.Errorf("stdout = %q", r.stdout)
		}
	})

	t.Run("reports every problem at once", func(t *testing.T) {
		cfg := writeConfig(t, `
version: 1
run: { duration: 10s, buckets: 1s }
http:
  executer: { rate: 5 }
  requests:
    - { name: probe, url: "http://127.0.0.1:8080/", methd: GET }
`)
		r := exec(t, "validate", "-c", cfg, "--output", "json")
		if r.code != errs.ExitUsage {
			t.Fatalf("exit %d, want 2", r.code)
		}
		doc := validateAgainst(t, compileSchema(t, "error"), r.stdout, "the error envelope")
		e, _ := doc["error"].(map[string]any)
		causes, _ := e["causes"].([]any)
		if len(causes) != 3 {
			t.Errorf("reported %d causes, want 3 so all three typos are fixed in one pass", len(causes))
		}
	})

	t.Run("contacts nothing", func(t *testing.T) {
		// A target that would fail preflight must not be touched by validate.
		srv := newServer(t, 0)
		dead := srv.URL
		srv.Close()
		cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 10s }
http:
  base_url: %q
  executor: { rate: 5 }
  requests:
    - { name: probe, url: "/api/items" }
`, dead))
		if r := exec(t, "validate", "-c", cfg); r.code != errs.ExitOK {
			t.Errorf("exit %d; validate must not contact the target", r.code)
		}
	})
}

// Every contract the binary claims to honour must be a schema it can actually emit,
// and each must be a valid schema in its own right.
func TestEverySchemaIsEmittableAndValid(t *testing.T) {
	for _, name := range []string{"config", "policy", "result", "digest", "events", "error"} {
		t.Run(name, func(t *testing.T) {
			r := exec(t, "schema", name)
			if r.code != errs.ExitOK {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			compileSchema(t, name)
		})
	}
	t.Run("unknown schema", func(t *testing.T) {
		r := exec(t, "schema", "nonsense")
		if r.code == errs.ExitOK {
			t.Error("an unknown schema name should not succeed")
		}
	})
}

func TestVersion(t *testing.T) {
	r := exec(t, "version", "--output", "json")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d", r.code)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil {
		t.Fatalf("version output is not JSON: %v", err)
	}
	if info["name"] != "tracepoint" {
		t.Errorf("name = %v", info["name"])
	}
}

// Logs and progress must never reach stdout in JSON mode: an agent parsing that stream
// has no way to tell a log line from the document.
func TestLogsNeverReachStdoutInJSONMode(t *testing.T) {
	srv := newServer(t, 0)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 1s, bucket: 500ms, seed: 1, timeout: 2s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 20, max_in_flight: 8 }
  requests:
    - { name: probe, url: "/api/items" }
`, srv.URL))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "debug")
	if r.stderr == "" {
		t.Error("debug logging produced nothing on stderr")
	}
	var doc any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout was polluted by logs: %v\n%s", err, truncate(r.stdout))
	}
}

// The same seed must reproduce the same choices, which is what makes a run
// re-runnable and a regression attributable.
func TestSeedIsRecordedAndReproducible(t *testing.T) {
	srv := newServer(t, 0)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 1s, seed: 1234, arrival: poisson, timeout: 2s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 30, max_in_flight: 8 }
  requests:
    - { name: a, weight: 3, url: "/api/items" }
    - { name: b, weight: 1, url: "/api/items" }
`, srv.URL))

	counts := func() (float64, float64) {
		r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
		if r.code != errs.ExitOK {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if seed := number(t, doc, "run", "seed"); seed != 1234 {
			t.Errorf("recorded seed = %v, want 1234", seed)
		}
		runners, _ := doc["runners"].([]any)
		rn, _ := runners[0].(map[string]any)
		return number(t, rn, "labels_summary", "a", "n"), number(t, rn, "labels_summary", "b", "n")
	}

	a1, b1 := counts()
	a2, b2 := counts()
	// Wall-clock jitter changes how many arrivals land before the run ends, so the
	// totals can differ by one; the weighted split must not.
	if a1+b1 == 0 || a2+b2 == 0 {
		t.Fatal("no operations recorded")
	}
	split1, split2 := a1/(a1+b1), a2/(a2+b2)
	if diff := split1 - split2; diff > 0.1 || diff < -0.1 {
		t.Errorf("the same seed produced different weighted splits: %.2f then %.2f", split1, split2)
	}
	if split1 < 0.6 || split1 > 0.9 {
		t.Errorf("3:1 weighting produced a split of %.2f, want about 0.75", split1)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
