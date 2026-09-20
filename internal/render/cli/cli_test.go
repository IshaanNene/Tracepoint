package cli_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	clirender "github.com/IshaanNene/Tracepoint/internal/render/cli"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var update = flag.Bool("update", false, "rewrite the golden files")

// golden compares rendered output against a recorded file.
//
// The renderer is a pure function of result.json, so the same document must always
// produce byte-identical output. That is what makes these files worth keeping: a diff
// here is a real change in what a user sees, not noise.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("writing the golden file: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\nrun `go test ./internal/render/cli/ -update` to create it", path, err)
	}
	if got != string(want) {
		t.Errorf("rendered output differs from %s\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

func render(t *testing.T, r *result.Result, opts clirender.Options) string {
	t.Helper()
	var buf bytes.Buffer
	if err := clirender.Render(&buf, r, opts); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

// healthyRun is a clean run that met its budgets.
func healthyRun() *result.Result {
	return &result.Result{
		SchemaVersion: result.SchemaVersion,
		Tool:          result.Tool{Name: "tracepoint", Version: "1.0.0"},
		Run: result.Run{
			ID: "20260920T120000Z-a1b2c3", Name: "checkout-baseline",
			Status: result.StatusCompleted, DurationMS: 180000, ElapsedMS: 180000,
			BucketMS: 1000, BucketCount: 180, Seed: 42,
		},
		Runners: []result.Runner{{
			Name: "http", Kind: "app",
			Summary: result.OpSummary{
				N: 36000, OK: 36000, ErrorRatio: 0, AchievedRPS: 200,
				Response:   result.Quantiles{P50: 12.4, P95: 38.1, P99: 61.2, Max: 140.9, Mean: 15.2},
				Service:    result.Quantiles{P50: 12.1, P95: 37.4, P99: 60.1, Max: 139.2, Mean: 14.9},
				ClientWait: result.Quantiles{P50: 0.3, P99: 1.1, Max: 4.2},
			},
			LabelSummaries: map[string]result.OpSummary{
				"list-items":  {N: 27000, AchievedRPS: 150, Response: result.Quantiles{P50: 10.1, P95: 24.0, P99: 31.2}},
				"create-item": {N: 9000, AchievedRPS: 50, Response: result.Quantiles{P50: 22.0, P95: 55.5, P99: 78.4}},
			},
			Executor: &result.ExecutorStats{
				Type: "arrival-rate", Offered: 36000, Dispatched: 36000, MaxInFlight: 400, PeakInFlight: 18,
			},
		}},
		Analysis: result.Analysis{
			Validity: result.Validity{State: result.ValidityValid},
			SLO: result.SLO{Pass: true, Checks: []result.SLOCheck{
				{Runner: "http", Metric: "p99", Budget: 250, Actual: 61.2, Pass: true},
				{Runner: "http", Metric: "error_rate", Budget: 0.01, Actual: 0, Pass: true},
			}},
			Verdict: result.Verdict{
				Bottleneck: result.BottleneckNone, Confidence: result.ConfidenceMedium,
				Summary: "Nothing stood out: the run stayed within its budgets and the generator kept to its schedule.",
				Evidence: []result.Evidence{{Kind: "sample_size",
					Text: "36000 operations at 200/s, p99 61.2ms, 0.00% errors"}},
				Caveat: result.StandingCaveat,
			},
		},
		Artifacts: &result.Artifacts{
			Result: "runs/20260920T120000Z-a1b2c3/result.json",
			Report: "runs/20260920T120000Z-a1b2c3/report.html",
		},
	}
}

// invalidRun is the case that matters most: the generator, not the target, set the
// pace, so the numbers must not be read as a measurement of the target.
func invalidRun() *result.Result {
	r := healthyRun()
	r.Run.Name = "capacity-probe"
	r.Runners[0].Summary.Response = result.Quantiles{P50: 410, P95: 980, P99: 1240, Max: 2010, Mean: 520}
	r.Runners[0].Summary.ClientWait = result.Quantiles{P50: 380, P99: 1100, Max: 1900}
	r.Runners[0].Summary.Errors = map[string]int64{"timeout": 214, "connection": 18}
	r.Runners[0].Summary.ErrorsTotal = 232
	r.Runners[0].Summary.ErrorRatio = 0.0064
	r.Runners[0].Executor.Dropped = 14200
	r.Runners[0].Executor.DroppedRatio = 0.394
	r.Analysis.Validity = result.Validity{
		State: result.ValidityInvalid,
		Findings: []result.Finding{{
			Code: "CLIENT_CAPPED", Severity: result.SeverityError,
			Message: "the http runner dropped 39.4% of arrivals while service time stayed flat: the ceiling was ours, not the target's",
			Fix:     "raise http.executor.max_in_flight to about 248 (Little's Law: 200 req/s x 1240ms service time)",
		}},
	}
	r.Analysis.SLO = result.SLO{Pass: false, Checks: []result.SLOCheck{
		{Runner: "http", Metric: "p99", Budget: 250, Actual: 1240, Pass: false},
	}}
	r.Analysis.Verdict = result.Verdict{
		Bottleneck: result.BottleneckClient, Confidence: result.ConfidenceHigh,
		Summary: "This run is invalid: the load generator, not the target, set the pace. " +
			"Its latency numbers describe TracePoint's own delay and must not be read as a measurement of the target.",
		Evidence: []result.Evidence{{Kind: "validity",
			Text: "the http runner dropped 39.4% of arrivals while service time stayed flat"}},
		Caveat:    result.StandingCaveat,
		NextSteps: []string{"raise http.executor.max_in_flight to about 248"},
	}
	return r
}

func TestRenderGolden(t *testing.T) {
	cases := map[string]struct {
		res  *result.Result
		opts clirender.Options
	}{
		"healthy":         {healthyRun(), clirender.Options{}},
		"healthy-verbose": {healthyRun(), clirender.Options{Verbose: true}},
		"invalid":         {invalidRun(), clirender.Options{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			golden(t, name, render(t, tc.res, tc.opts))
		})
	}
}

// The same document must render identically every time, or golden files mean nothing
// and a regenerated report cannot be diffed.
func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	r := healthyRun()
	first := render(t, r, clirender.Options{Verbose: true})
	for range 20 {
		if got := render(t, r, clirender.Options{Verbose: true}); got != first {
			t.Fatal("rendering the same result twice produced different output; map iteration order is leaking into it")
		}
	}
}

// Colour is opt-in. Piping the output somewhere must not change its shape, only its
// escape codes, because a terminal escape in a log file is noise.
func TestColourAddsOnlyEscapes(t *testing.T) {
	t.Parallel()
	r := healthyRun()
	plain := render(t, r, clirender.Options{})
	coloured := render(t, r, clirender.Options{Colour: true})

	if !strings.Contains(coloured, "\x1b[") {
		t.Error("colour was requested but no escapes were emitted")
	}
	if strings.Contains(plain, "\x1b[") {
		t.Error("escapes leaked into plain output")
	}
	// Alignment must survive colour: padding has to be applied to the visible text,
	// not to a string that already contains escape codes.
	if stripANSI(coloured) != plain {
		t.Errorf("colour changed the text, not just its presentation\n--- plain ---\n%s\n--- coloured, escapes stripped ---\n%s",
			plain, stripANSI(coloured))
	}
}

// flatten collapses whitespace so an assertion about wording is not defeated by where
// the renderer happened to wrap a line.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// An invalid run must say so before it says anything else, and a reader skimming the
// top must not meet a latency figure first.
func TestInvalidRunLeadsWithItsVerdict(t *testing.T) {
	t.Parallel()
	out := render(t, invalidRun(), clirender.Options{})
	runIdx := strings.Index(out, "INVALID")
	tableIdx := strings.Index(out, "RUNNER")
	if runIdx < 0 {
		t.Fatal("an invalid run did not say INVALID")
	}
	if runIdx > tableIdx {
		t.Error("the latency table appears before the invalidity notice")
	}
	if !strings.Contains(out, "Little's Law") {
		t.Error("the fix should name the concrete change to make")
	}
	if !strings.Contains(flatten(out), "must not be read as a measurement of the target") {
		t.Error("the verdict must warn against reading an invalid run's numbers")
	}
}

// Every verdict carries the standing caveat, because the tool has no causal evidence
// and says so each time it speaks.
func TestEveryVerdictCarriesTheCaveat(t *testing.T) {
	t.Parallel()
	for _, r := range []*result.Result{healthyRun(), invalidRun()} {
		if out := render(t, r, clirender.Options{}); !strings.Contains(out, "association, not causation") {
			t.Errorf("a verdict was rendered without the standing caveat:\n%s", out)
		}
	}
}

// Client-side waiting is called out when it is a material share of the reported
// latency, because at that point the number describes the generator, not the target.
func TestClientWaitIsCalledOutWhenItDominates(t *testing.T) {
	t.Parallel()
	if out := render(t, invalidRun(), clirender.Options{}); !strings.Contains(out, "waiting inside the generator") {
		t.Errorf("client wait was 1100ms of a 1240ms p99 and was not called out:\n%s", out)
	}
	if out := render(t, healthyRun(), clirender.Options{}); strings.Contains(out, "waiting inside the generator") {
		t.Error("client wait was negligible but was called out anyway")
	}
}

func TestRunWithNoBudgetsSaysSo(t *testing.T) {
	t.Parallel()
	r := healthyRun()
	r.Analysis.SLO = result.SLO{Pass: true}
	out := render(t, r, clirender.Options{})
	if !strings.Contains(out, "no budgets configured") {
		t.Error("a run with no budgets should say so rather than implying it passed something")
	}
}
