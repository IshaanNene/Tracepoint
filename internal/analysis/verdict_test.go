package analysis

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func locksTelemetry(n, from, to int) *result.Telemetry {
	var samples []map[string]any
	for i := range n {
		v := 0.0
		if i >= from && i <= to {
			v = 9
		}
		samples = append(samples, map[string]any{"t_ms": float64(i * 1000), "locks_waiting": v})
	}
	return &result.Telemetry{Postgres: &result.SamplerSeries{Available: true, Samples: samples}}
}

func TestVerdictDatabase(t *testing.T) {
	r := newScenario(90).
		set("db", 30, 34, 900).set("http", 31, 35, 950).
		set("db", 60, 63, 800).set("http", 60, 63, 900).result()
	r.Telemetry = locksTelemetry(90, 30, 34)
	a := analyse(t, r)
	v := a.Verdict
	if v.Bottleneck != result.BottleneckDB {
		t.Fatalf("bottleneck = %s, want db\n%s", v.Bottleneck, v.Summary)
	}
	// Sufficient samples, two agreeing incidents and lock waits: all three points.
	if v.Confidence != result.ConfidenceHigh {
		t.Fatalf("confidence = %s, want high", v.Confidence)
	}
	for _, want := range []string{"database tier", "all 2 incidents that reached users", "postgres locks_waiting"} {
		if !strings.Contains(v.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, v.Summary)
		}
	}
	if v.Caveat != result.StandingCaveat {
		t.Fatalf("caveat = %q", v.Caveat)
	}
	kinds := map[string]int{}
	for _, e := range v.Evidence {
		kinds[e.Kind]++
	}
	if kinds["incident"] != 2 || kinds["telemetry"] != 1 || kinds["sample_size"] != 1 {
		t.Fatalf("evidence kinds = %v", kinds)
	}
	if len(v.NextSteps) == 0 || !strings.Contains(v.NextSteps[0], "30s-36s") {
		t.Fatalf("next steps should name the incident window: %v", v.NextSteps)
	}
}

// One uncorroborated incident is a lead worth following, not a finding.
func TestVerdictConfidenceDropsWithoutCorroboration(t *testing.T) {
	r := newScenario(90).set("db", 30, 34, 900).set("http", 31, 35, 950).result()
	v := analyse(t, r).Verdict
	if v.Bottleneck != result.BottleneckDB {
		t.Fatalf("bottleneck = %s", v.Bottleneck)
	}
	if v.Confidence == result.ConfidenceHigh {
		t.Fatalf("confidence = high on one uncorroborated incident")
	}
	if !strings.Contains(v.Summary, "No server-side telemetry corroborated it") {
		t.Fatalf("summary should say nothing corroborated it:\n%s", v.Summary)
	}
}

func TestVerdictApp(t *testing.T) {
	r := newScenario(90).set("http", 30, 34, 900).result()
	v := analyse(t, r).Verdict
	if v.Bottleneck != result.BottleneckApp {
		t.Fatalf("bottleneck = %s, want app", v.Bottleneck)
	}
	if !strings.Contains(v.Summary, "application tier itself") {
		t.Fatalf("summary:\n%s", v.Summary)
	}
}

func TestVerdictRedis(t *testing.T) {
	r := newScenario(90).set("redis", 40, 42, 2500).set("http", 40, 42, 2600).result()
	if v := analyse(t, r).Verdict; v.Bottleneck != result.BottleneckRedis {
		t.Fatalf("bottleneck = %s, want redis", v.Bottleneck)
	}
}

func TestVerdictClient(t *testing.T) {
	r := newScenario(90).set("http", 30, 60, 300).waitAt("http", 30, 60, 4000).result()
	if v := analyse(t, r).Verdict; v.Bottleneck != result.BottleneckClient {
		t.Fatalf("bottleneck = %s, want client", v.Bottleneck)
	}
}

func TestVerdictUnobserved(t *testing.T) {
	r := newScenario(90).drop("db").drop("redis").set("http", 30, 34, 900).result()
	v := analyse(t, r).Verdict
	if v.Bottleneck != result.BottleneckInconclusive || v.Confidence != result.ConfidenceLow {
		t.Fatalf("got %s/%s, want inconclusive/low", v.Bottleneck, v.Confidence)
	}
}

func TestVerdictTie(t *testing.T) {
	r := newScenario(90).set("db", 30, 33, 400).set("redis", 30, 33, 400).set("http", 30, 33, 900).result()
	v := analyse(t, r).Verdict
	if v.Bottleneck != result.BottleneckInconclusive || v.Confidence != result.ConfidenceLow {
		t.Fatalf("got %s/%s, want inconclusive/low", v.Bottleneck, v.Confidence)
	}
	if !strings.Contains(v.Summary, "db and redis") {
		t.Fatalf("summary should name both tiers:\n%s", v.Summary)
	}
}

// Two different tiers each named alone by an equal share of incidents is a tie at the
// level of the run, not a coin toss.
func TestVerdictRunLevelTie(t *testing.T) {
	r := newScenario(90).
		set("db", 20, 23, 900).set("http", 20, 23, 950).
		set("redis", 60, 63, 900).set("http", 60, 63, 950).result()
	if v := analyse(t, r).Verdict; v.Bottleneck != result.BottleneckInconclusive {
		t.Fatalf("bottleneck = %s, want inconclusive", v.Bottleneck)
	}
}

func TestVerdictMasked(t *testing.T) {
	r := newScenario(90).set("db", 30, 34, 900).result()
	v := analyse(t, r).Verdict
	if v.Bottleneck != result.BottleneckDB {
		t.Fatalf("bottleneck = %s, want db", v.Bottleneck)
	}
	if !strings.Contains(v.Summary, "has not yet reached users") {
		t.Fatalf("summary:\n%s", v.Summary)
	}
	if v.Confidence == result.ConfidenceHigh {
		t.Fatalf("a masked bottleneck is a warning, never high confidence")
	}
}

// A budget missed evenly, with no episode, names no tier unless the whole-run
// correlation leans strongly.
func TestVerdictUniformBreach(t *testing.T) {
	budget := config.Duration(10 * time.Millisecond)
	r := newScenario(90).result()
	r.Runners[0].Summary.Response.P95 = 25
	// A p95 budget leaves the hot-bucket threshold at its 100ms default, which the
	// steady 20ms series never crosses: slow against the budget, but evenly.
	a := Analyse(r, Inputs{SLO: config.SLO{HTTP: config.SLOTarget{P95: &budget}}})
	if a.SLO.Pass {
		t.Fatalf("the budget should have failed")
	}
	v := a.Verdict
	// The synthetic tiers wobble in lockstep, so rho is 1 - and it still must not be
	// promoted to an answer on its own.
	if v.Bottleneck != result.BottleneckInconclusive || !strings.Contains(v.Summary, "slow evenly") ||
		!strings.Contains(v.Summary, "not enough to name it") {
		t.Fatalf("verdict = %+v", v)
	}
	found := false
	for _, e := range v.Evidence {
		if e.Kind == "slo" && strings.Contains(e.Text, "http p95 was 25.0ms against a budget of 10.0ms") {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence lacks the breach: %+v", v.Evidence)
	}
}

func TestVerdictQuiet(t *testing.T) {
	v := analyse(t, newScenario(90).result()).Verdict
	if v.Bottleneck != result.BottleneckNone || v.Confidence != result.ConfidenceHigh {
		t.Fatalf("got %s/%s, want none/high on 90 clean sufficient buckets", v.Bottleneck, v.Confidence)
	}
}

func TestVerdictDegradedCapsConfidence(t *testing.T) {
	r := newScenario(90).result()
	r.Runners[0].Targets = []result.Target{{Host: "localhost", Scope: result.ScopeLoopback}}
	a := analyse(t, r)
	if a.Validity.State != result.ValidityDegraded {
		t.Fatalf("validity = %s", a.Validity.State)
	}
	if a.Verdict.Confidence != result.ConfidenceMedium {
		t.Fatalf("confidence = %s, want capped at medium", a.Verdict.Confidence)
	}
}

func TestVerdictNoOperations(t *testing.T) {
	r := newScenario(10).result()
	r.Runners[0].Summary.N = 0
	if v := analyse(t, r).Verdict; v.Bottleneck != result.BottleneckInconclusive {
		t.Fatalf("bottleneck = %s", v.Bottleneck)
	}
}

func TestVerdictInvalid(t *testing.T) {
	r := newScenario(30).result()
	r.Runners[0].Executor = &result.ExecutorStats{DispatchLagMS: result.Quantiles{P99: 400}, MaxInFlight: 10}
	a := analyse(t, r)
	if a.Validity.State != result.ValidityInvalid || a.Verdict.Bottleneck != result.BottleneckClient {
		t.Fatalf("got %s / %s", a.Validity.State, a.Verdict.Bottleneck)
	}
	if !strings.Contains(a.Verdict.Summary, "must not be read") {
		t.Fatalf("summary:\n%s", a.Verdict.Summary)
	}
}
