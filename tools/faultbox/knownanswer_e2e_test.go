//go:build e2e

package faultbox_test

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

	"github.com/IshaanNene/Tracepoint/internal/cli"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
	"github.com/IshaanNene/Tracepoint/internal/testenv"
	"github.com/IshaanNene/Tracepoint/tools/faultbox"
)

// The known-answer suite (§10). Each scenario injects a fault whose cause is known in
// advance, at a known offset from the run start, and asserts that TracePoint names it:
// the right incident class, the right culprit, a window within one bucket of when the
// fault actually held, the right verdict and the right exit code. A methodology that
// cannot pass its own fault injection is not a methodology (ADR-005).

// base is the three-tier configuration every scenario starts from. The database probe
// reads both the table the endpoint reads and a table only the probe reads, so one
// configuration serves both lock scenarios. Rates keep every one-second bucket above
// min_samples for every tier.
const base = `
version: 1
run: { duration: 40s, bucket: %s, warmup: 5s, seed: 11, timeout: 15s }
slo:
  http: { p99: 250ms, error_rate: 0.01 }
http:
  base_url: %q
  executor: { rate: %d, max_in_flight: %d }
  requests:
    - { name: item, url: "/api/items/{{randInt 1 1000}}" }
db:
  driver: postgres
  dsn: %q
  executor: { rate: 40, max_in_flight: 200 }
  queries:
    - { name: item-by-id, type: read, sql: "SELECT title FROM items WHERE id = $1", args: ["{{randInt 1 1000}}"] }
    - { name: probe-by-id, type: read, sql: "SELECT title FROM probe_only WHERE id = $1", args: ["{{randInt 1 1000}}"] }
redis:
  addr: %q
  executor: { rate: 50, max_in_flight: 200 }
  commands:
    - { name: get-item, type: read, cmd: [GET, "item:{{randInt 1 1000}}"] }
telemetry: { postgres: true, redis: true }
`

type env struct {
	appURL, adminURL, dsn, redis string
	dir                          string
}

type scenario struct {
	name  string
	fault string // JSON for POST /admin/faults, or empty
	// bucket, rate and inFlight shape the load. The client-limited scenario starves
	// the generator on purpose, and needs wide buckets for each to hold enough
	// completed operations to count as evidence.
	bucket   string
	rate     int
	inFlight int
	exit     int
	check    func(t *testing.T, r *result.Result, window *span)
}

// span is the bucket range a fault actually held for, from faultbox's own record.
type span struct{ start, end int }

func TestKnownAnswers(t *testing.T) {
	e := setup(t)

	scenarios := []scenario{
		{
			name: "clean run has no incidents",
			exit: errs.ExitOK,
			check: func(t *testing.T, r *result.Result, _ *span) {
				if n := len(r.Analysis.Incidents); n != 0 {
					t.Errorf("a clean run produced %d incident(s): %+v", n, r.Analysis.Incidents)
				}
				expectVerdict(t, r, result.BottleneckNone)
			},
		},
		{
			name:  "lock on a table the endpoint and the probe share is correlated, db",
			fault: `{"kind":"pg_lock","table":"items","at":"20s","duration":"5s"}`,
			exit:  errs.ExitBreach,
			check: func(t *testing.T, r *result.Result, w *span) {
				inc := expectIncident(t, r, result.IncidentCorrelated, w)
				expectCulprit(t, inc, "db")
				expectSignal(t, inc, "postgres")
				expectVerdict(t, r, result.BottleneckDB)
			},
		},
		{
			name:  "lock on a table only the probe reads is storage_only",
			fault: `{"kind":"pg_lock","table":"probe_only","at":"20s","duration":"5s"}`,
			exit:  errs.ExitOK,
			check: func(t *testing.T, r *result.Result, w *span) {
				inc := expectIncident(t, r, result.IncidentStorageOnly, w)
				expectHot(t, inc, "db")
				for _, i := range r.Analysis.Incidents {
					if i.Class == result.IncidentCorrelated || i.Class == result.IncidentAppOnly {
						t.Errorf("users were not affected, yet %s is %s", i.ID, i.Class)
					}
				}
			},
		},
		{
			name:  "handler delay is app_only",
			fault: `{"kind":"delay","delay":"400ms","at":"20s","duration":"5s"}`,
			exit:  errs.ExitBreach,
			check: func(t *testing.T, r *result.Result, w *span) {
				expectIncident(t, r, result.IncidentAppOnly, w)
				expectVerdict(t, r, result.BottleneckApp)
			},
		},
		{
			name:  "redis DEBUG SLEEP is correlated, redis",
			fault: `{"kind":"redis_sleep","at":"20s","duration":"3s"}`,
			exit:  errs.ExitBreach,
			check: func(t *testing.T, r *result.Result, w *span) {
				inc := expectIncident(t, r, result.IncidentCorrelated, w)
				expectCulprit(t, inc, "redis")
				expectSignal(t, inc, "redis")
				expectVerdict(t, r, result.BottleneckRedis)
			},
		},
		{
			name:     "a starved generator is client_limited and the target is not blamed",
			fault:    `{"kind":"delay","delay":"300ms"}`,
			bucket:   "5s",
			rate:     20,
			inFlight: 2,
			exit:     errs.ExitInvalid,
			check: func(t *testing.T, r *result.Result, _ *span) {
				if r.Analysis.Validity.State != result.ValidityInvalid {
					t.Errorf("validity = %s, want invalid", r.Analysis.Validity.State)
				}
				found := false
				for _, inc := range r.Analysis.Incidents {
					switch inc.Class {
					case result.IncidentClientLimited:
						found = true
					case result.IncidentCorrelated, result.IncidentAppOnly, result.IncidentStorageOnly:
						t.Errorf("the target was blamed: %s is %s", inc.ID, inc.Class)
					}
				}
				if !found {
					t.Errorf("no client_limited incident: %+v", r.Analysis.Incidents)
				}
				expectVerdict(t, r, result.BottleneckClient)
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			r, w := e.run(t, sc)
			sc.check(t, r, w)
		})
	}
}

func setup(t *testing.T) *env {
	t.Helper()
	dsn := testenv.Postgres(t)
	redisAddr := testenv.Redis(t, true)

	ctx := context.Background()
	app, err := faultbox.New(ctx, faultbox.Config{DSN: dsn, RedisAddr: redisAddr, PoolSize: 200})
	if err != nil {
		t.Fatalf("faultbox: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if err := app.Setup(ctx); err != nil {
		t.Fatalf("faultbox setup: %v", err)
	}
	appSrv := httptest.NewServer(app.Handler())
	t.Cleanup(appSrv.Close)
	adminSrv := httptest.NewServer(app.AdminHandler())
	t.Cleanup(adminSrv.Close)
	return &env{appURL: appSrv.URL, adminURL: adminSrv.URL, dsn: dsn, redis: redisAddr, dir: t.TempDir()}
}

func (e *env) run(t *testing.T, sc scenario) (*result.Result, *span) {
	t.Helper()
	bucket, rate, inFlight := sc.bucket, sc.rate, sc.inFlight
	if bucket == "" {
		bucket = "1s"
	}
	if rate == 0 {
		rate = 50
	}
	if inFlight == 0 {
		inFlight = 400
	}

	e.admin(t, http.MethodDelete, "", nil)
	if sc.fault != "" {
		e.admin(t, http.MethodPost, sc.fault, nil)
	}

	cfgPath := filepath.Join(e.dir, "tracepoint.yaml")
	cfg := fmt.Sprintf(base, bucket, e.appURL, rate, inFlight, e.dsn, e.redis)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(e.dir, strings.ReplaceAll(t.Name(), "/", "_")+".json")

	var stdout, stderr bytes.Buffer
	code := cli.Execute(context.Background(), cli.Env{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		Args:   []string{"run", "-c", cfgPath, "--result-path", resultPath, "--output", "json", "--log-level", "warn"},
		Lookup: func(string) (string, bool) { return "", false },
		// Every run writes a directory; keep it with the scenario, not in the tree.
		RunRoot: filepath.Join(e.dir, "runs"),
	})
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("no result (exit %d): %v\nstderr: %s\nstdout: %s", code, err, stderr.String(), stdout.String())
	}
	schematest.Validate(t, schemas.Result, raw)
	r, err := result.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever assertion fails, and however, the analysis it judged is in the log.
	t.Cleanup(func() {
		if t.Failed() {
			dumpAnalysis(t, r)
		}
	})
	if code != sc.exit {
		t.Errorf("exit %d, want %d\nstderr: %s", code, sc.exit, stderr.String())
	}

	// The digest of every known answer must itself be a valid contract.
	d, err := result.BuildDigest(r, result.DigestOptions{}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, schemas.Digest, d)

	var listed struct {
		Faults []faultbox.Fault `json:"faults"`
	}
	e.admin(t, http.MethodGet, "", &listed)
	var w *span
	for _, f := range listed.Faults {
		if f.Error != "" {
			t.Fatalf("the fault itself failed: %s", f.Error)
		}
		if f.BeganMS != nil && f.EndedMS != nil {
			w = &span{int(*f.BeganMS / r.Run.BucketMS), int(*f.EndedMS / r.Run.BucketMS)}
			t.Logf("fault %s held from %.0fms to %.0fms: buckets %d..%d", f.Kind, *f.BeganMS, *f.EndedMS, w.start, w.end)
		}
	}
	return r, w
}

func (e *env) admin(t *testing.T, method, body string, out any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.adminURL+"/admin/faults", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin %s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		t.Fatalf("admin %s: %s", method, resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

// expectIncident finds an incident of the class whose edges are each within one
// bucket of when the fault actually held (§10: "within ±1 bucket").
func expectIncident(t *testing.T, r *result.Result, class string, w *span) result.Incident {
	t.Helper()
	if w == nil {
		t.Fatalf("faultbox recorded no window for the fault")
	}
	for _, inc := range r.Analysis.Incidents {
		if inc.Class != class {
			continue
		}
		if abs(inc.StartIndex-w.start) <= 1 && abs(inc.EndIndex-w.end) <= 1 {
			return inc
		}
	}
	t.Fatalf("no %s incident within one bucket of %d..%d", class, w.start, w.end)
	return result.Incident{}
}

func expectCulprit(t *testing.T, inc result.Incident, runner string) {
	t.Helper()
	if inc.Culprit == nil || inc.Culprit.Runner != runner || len(inc.Culprit.TiedWith) > 0 {
		t.Errorf("%s culprit = %+v, want %s alone", inc.ID, inc.Culprit, runner)
	}
}

func expectHot(t *testing.T, inc result.Incident, runner string) {
	t.Helper()
	for _, rn := range inc.Runners {
		if rn.Name == runner && rn.Hot {
			return
		}
	}
	t.Errorf("%s: %s was not hot", inc.ID, runner)
}

func expectSignal(t *testing.T, inc result.Incident, source string) {
	t.Helper()
	for _, s := range inc.Telemetry {
		if s.Source == source {
			return
		}
	}
	t.Errorf("%s: no %s telemetry corroborated it: %+v", inc.ID, source, inc.Telemetry)
}

func expectVerdict(t *testing.T, r *result.Result, bottleneck string) {
	t.Helper()
	if got := r.Analysis.Verdict.Bottleneck; got != bottleneck {
		t.Errorf("verdict = %s, want %s: %s", got, bottleneck, r.Analysis.Verdict.Summary)
	}
}

func dumpAnalysis(t *testing.T, r *result.Result) {
	t.Helper()
	b, _ := json.MarshalIndent(struct {
		Validity  result.Validity               `json:"validity"`
		Hot       map[string][]result.HotBucket `json:"hot_buckets"`
		Incidents []result.Incident             `json:"incidents"`
		Verdict   result.Verdict                `json:"verdict"`
	}{r.Analysis.Validity, r.Analysis.HotBuckets, r.Analysis.Incidents, r.Analysis.Verdict}, "", "  ")
	t.Logf("analysis:\n%s", b)

	// The generator's own health around each incident: a stall shows here as dispatch
	// lag rising on every runner at once while GC pauses and scheduler latency stay
	// flat - the whole process paused, not the target slowed.
	if r.Telemetry == nil || r.Telemetry.Generator == nil {
		return
	}
	for _, inc := range r.Analysis.Incidents {
		// Each runner's own view of the same seconds: whether the time was spent in
		// the target (service) or before it (client wait).
		for _, rn := range r.Runners {
			for _, b := range rn.Buckets {
				if b.Index < inc.StartIndex-2 || b.Index > inc.EndIndex+2 {
					continue
				}
				t.Logf("%s bucket %d: n=%d service p50=%.3f p99=%.3f max=%.3f client_wait p99=%.3f in_flight_max=%d",
					rn.Name, b.Index, b.N, b.Service.P50, b.Service.P99, b.Service.Max, b.ClientWait.P99, b.InFlightMax)
			}
		}
		for _, s := range r.Telemetry.Generator.Samples {
			ms, ok := s["t_ms"].(float64)
			if !ok || ms < inc.StartS*1000-2000 || ms > inc.EndS*1000+2000 {
				continue
			}
			if line, err := json.Marshal(s); err == nil {
				t.Logf("generator near %s: %s", inc.ID, line)
			}
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
