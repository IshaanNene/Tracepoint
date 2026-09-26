package compare

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/DataDog/sketches-go/ddsketch"
	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// tier is one runner's observations for a synthetic run.
type tier struct {
	name   string
	values []float64 // response times, ms
	errors int
	labels map[string][]float64
}

func sketchOf(t *testing.T, vs []float64) result.Sketch {
	t.Helper()
	s, err := ddsketch.NewDefaultDDSketch(metrics.DefaultRelativeAccuracy)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if err := s.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	e := metrics.EncodeSketch(s, metrics.DefaultRelativeAccuracy)
	return result.Sketch{Encoding: e.Encoding, RelativeAccuracy: e.RelativeAccuracy, Count: e.Count, Data: e.Data}
}

func exactQ(vs []float64, q float64) float64 {
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	return s[int(math.Round(q*float64(len(s)-1)))]
}

func summary(vs []float64, failed int, secs float64) result.OpSummary {
	n := int64(len(vs))
	return result.OpSummary{
		N: n, OK: n - int64(failed), ErrorsTotal: int64(failed), ErrorRatio: float64(failed) / float64(n),
		AchievedRPS: float64(n) / secs,
		Response:    result.Quantiles{P50: exactQ(vs, 0.5), P95: exactQ(vs, 0.95), P99: exactQ(vs, 0.99)},
	}
}

func run(t *testing.T, id string, tiers ...tier) *result.Result {
	t.Helper()
	r := &result.Result{
		SchemaVersion: result.SchemaVersion,
		Run:           result.Run{ID: id, Status: result.StatusCompleted, BucketMS: 1000, DurationMS: 60000},
		Config:        &result.Config{Effective: map[string]any{"run": map[string]any{"duration": "1m"}}},
		Analysis:      result.Analysis{Validity: result.Validity{State: result.ValidityValid}},
	}
	for _, tr := range tiers {
		kind := "storage"
		if tr.name == "http" {
			kind = "app"
		}
		rn := result.Runner{Name: tr.name, Kind: kind, Summary: summary(tr.values, tr.errors, 60),
			Sketches: map[string]result.Sketch{metrics.RunnerSketchKey: sketchOf(t, tr.values)}}
		for label, vs := range tr.labels {
			if rn.LabelSummaries == nil {
				rn.LabelSummaries = map[string]result.OpSummary{}
			}
			rn.LabelSummaries[label] = summary(vs, 0, 60)
			rn.Sketches[label] = sketchOf(t, vs)
		}
		r.Runners = append(r.Runners, rn)
	}
	return r
}

// latencies draws n response times around a 20ms median with a long tail, shifted
// by add.
func latencies(seed uint64, n int, add float64) []float64 {
	rng := rand.New(rand.NewPCG(seed, 7))
	out := make([]float64, n)
	for i := range out {
		out[i] = 20*math.Exp(0.4*rng.NormFloat64()) + add
	}
	return out
}

// mustCompare compares, and holds every report to its schema.
func mustCompare(t *testing.T, b, c *result.Result, opts Options) *Report {
	t.Helper()
	rep, err := Compare(b, c, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, schemas.Compare, raw)
	return rep
}

func delta(rep *Report, runner, label string) *Delta {
	for i := range rep.Runners {
		if d := &rep.Runners[i]; d.Runner == runner && d.Label == label {
			return d
		}
	}
	for i := range rep.Labels {
		if d := &rep.Labels[i]; d.Runner == runner && d.Label == label {
			return d
		}
	}
	return nil
}

func gate(rep *Report, name, runner string) *Gate {
	for i := range rep.Gates {
		if g := &rep.Gates[i]; g.Name == name && g.Runner == runner {
			return g
		}
	}
	return nil
}

// A/A: two runs of the same system differ only by sampling, and the intervals are
// built so that sampling alone is not called a change.
func TestAAShowsNoChange(t *testing.T) {
	for seed := range uint64(20) {
		b := run(t, "a", tier{name: "http", values: latencies(seed, 6000, 0)})
		c := run(t, "b", tier{name: "http", values: latencies(seed+100, 6000, 0)})
		rep := mustCompare(t, b, c, Options{})
		if rep.Regression || rep.ExitCode() != errs.ExitOK {
			t.Fatalf("seed %d: A/A reported a regression: %s", seed, rep.Summary)
		}
		if d := delta(rep, "http", ""); d.Change != ChangeUnchanged {
			t.Fatalf("seed %d: A/A p99 %.1f -> %.1f called %s (CIs %v %v)", seed, d.Baseline.P99MS, d.Current.P99MS, d.Change, d.P99CI.Baseline, d.P99CI.Current)
		}
	}
}

// +50ms on every request is a significant regression that crosses the relative gate.
func TestShiftIsARegression(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 6000, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 6000, 50)})
	rep := mustCompare(t, b, c, Options{})
	d := delta(rep, "http", "")
	if d.Change != ChangeSlower || d.P99MS < 40 || d.P99Pct < 50 {
		t.Fatalf("delta = %+v", d)
	}
	g := gate(rep, GateRelative, "http")
	if g == nil || g.Pass || !rep.Regression || rep.ExitCode() != errs.ExitBreach {
		t.Fatalf("gate %+v, regression %v, exit %d", g, rep.Regression, rep.ExitCode())
	}
	if !strings.Contains(rep.Summary, "http") || !strings.Contains(strings.ToLower(rep.Summary), "regression") {
		t.Fatalf("summary = %q", rep.Summary)
	}
	if code := rep.ErrorCode(); code != errs.CodeCompareRegression {
		t.Fatalf("code = %s", code)
	}
	// The reverse is an improvement, and no gate is crossed.
	rev := mustCompare(t, c, b, Options{})
	if d := delta(rev, "http", ""); d.Change != ChangeFaster || rev.Regression {
		t.Fatalf("reverse: %+v, regression %v", d, rev.Regression)
	}
}

// A significant but small rise is reported, and does not cross the relative gate,
// which needs more than 10% and more than 5ms.
func TestSmallSignificantRiseIsNotAGate(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 200000, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 200000, 3)})
	rep := mustCompare(t, b, c, Options{})
	d := delta(rep, "http", "")
	if d.Change != ChangeSlower {
		t.Fatalf("a 3ms shift over 200k samples should be significant: %+v", d)
	}
	if g := gate(rep, GateRelative, "http"); g == nil || !g.Pass || rep.Regression {
		t.Fatalf("gate %+v", g)
	}
}

// Too few samples for an interval: the change cannot be called, and a rise that
// would cross the gate cannot be cleared either.
func TestInsufficientData(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 60, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 60, 80)})
	rep := mustCompare(t, b, c, Options{})
	d := delta(rep, "http", "")
	if d.Change != ChangeInsufficient || d.P99CI.Baseline != nil {
		t.Fatalf("delta = %+v", d)
	}
	g := gate(rep, GateRelative, "http")
	if g == nil || g.Pass || !g.Insufficient {
		t.Fatalf("gate %+v", g)
	}
	if rep.ExitCode() != errs.ExitBreach || rep.ErrorCode() != errs.CodeCompareInsufficient {
		t.Fatalf("exit %d code %s", rep.ExitCode(), rep.ErrorCode())
	}
	// A small run with no rise worth gating is not a failure. (Two independent draws
	// of 60 would not do: a p99 of 60 is in effect the maximum, and moves by more than
	// the gate between draws - which is exactly what this rule refuses to clear.)
	same := mustCompare(t, b, b, Options{})
	if same.Regression {
		t.Fatalf("summary = %s", same.Summary)
	}
}

// Crossing the budget fails even without significance: the promise was broken.
func TestBudgetCrossing(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 6000, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 6000, 0)})
	budget := (b.Runners[0].Summary.Response.P99 + c.Runners[0].Summary.Response.P99) / 2
	b.Runners[0].Summary.Response.P99 = budget - 1
	c.Runners[0].Summary.Response.P99 = budget + 1
	c.Analysis.SLO = result.SLO{Checks: []result.SLOCheck{{Runner: "http", Metric: "p99", Budget: budget}}}
	rep := mustCompare(t, b, c, Options{})
	if g := gate(rep, GateBudget, "http"); g == nil || g.Pass || !rep.Regression {
		t.Fatalf("gate %+v", g)
	}
	// Over the budget in both runs is not a crossing.
	b.Runners[0].Summary.Response.P99 = budget + 2
	if g := gate(mustCompare(t, b, c, Options{}), GateBudget, "http"); g == nil || !g.Pass {
		t.Fatalf("gate %+v", g)
	}
}

func TestErrorRateGate(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 6000, 0), errors: 6})
	c := run(t, "b", tier{name: "http", values: latencies(2, 6000, 0), errors: 60})
	rep := mustCompare(t, b, c, Options{})
	g := gate(rep, GateErrorRate, "http")
	if g == nil || g.Pass || math.Abs(delta(rep, "http", "").ErrorPP-0.9) > 1e-9 {
		t.Fatalf("gate %+v, delta %+v", g, delta(rep, "http", ""))
	}
	c.Runners[0].Summary.ErrorRatio = 0.004 // +0.3pp
	if g := gate(mustCompare(t, b, c, Options{}), GateErrorRate, "http"); g == nil || !g.Pass {
		t.Fatalf("0.3pp crossed a 0.5pp gate: %+v", g)
	}
}

// Gates are configurable.
func TestGatesAreConfigurable(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 200000, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 200000, 3)})
	rep := mustCompare(t, b, c, Options{Gates: Gates{RelativePct: 2, RelativeMS: 1, ErrorPP: 0.5}})
	if g := gate(rep, GateRelative, "http"); g == nil || g.Pass {
		t.Fatalf("a 2%%/1ms gate let a 3ms rise through: %+v", g)
	}
}

// Every tier and every label is compared.
func TestLabelsAndTiers(t *testing.T) {
	b := run(t, "a",
		tier{name: "http", values: latencies(1, 6000, 0), labels: map[string][]float64{"items": latencies(3, 3000, 0)}},
		tier{name: "db", values: latencies(5, 6000, 0)})
	c := run(t, "b",
		tier{name: "http", values: latencies(2, 6000, 0), labels: map[string][]float64{"items": latencies(4, 3000, 60)}},
		tier{name: "db", values: latencies(6, 6000, 0)})
	rep := mustCompare(t, b, c, Options{})
	if d := delta(rep, "http", "items"); d == nil || d.Change != ChangeSlower {
		t.Fatalf("label delta = %+v", d)
	}
	if delta(rep, "db", "") == nil {
		t.Fatal("no db delta")
	}
}

// A runner in only one run is reported, not silently dropped.
func TestMissingRunner(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 6000, 0)}, tier{name: "db", values: latencies(5, 6000, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 6000, 0)})
	rep := mustCompare(t, b, c, Options{})
	found := false
	for _, w := range rep.Warnings {
		found = found || (w.Code == CodeRunnerMissing && strings.Contains(w.Message, "db"))
	}
	if !found {
		t.Fatalf("warnings = %+v", rep.Warnings)
	}
}

func TestConfigDiff(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 600, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 600, 0)})
	b.Config.Effective = map[string]any{"run": map[string]any{"duration": "1m", "seed": 1.0}, "http": map[string]any{"executor": map[string]any{"rate": 50.0}}}
	c.Config.Effective = map[string]any{"run": map[string]any{"duration": "1m", "seed": 2.0}, "http": map[string]any{"executor": map[string]any{"rate": 80.0, "max_in_flight": 64.0}}}
	rep := mustCompare(t, b, c, Options{})
	want := []string{"http.executor.max_in_flight", "http.executor.rate", "run.seed"}
	if strings.Join(rep.ConfigDiff, ",") != strings.Join(want, ",") {
		t.Fatalf("config diff = %v, want %v", rep.ConfigDiff, want)
	}
	found := false
	for _, w := range rep.Warnings {
		found = found || w.Code == CodeConfigDiffers
	}
	if !found {
		t.Fatal("no warning for differing configurations")
	}
}

func TestInvalidInputsAreFlagged(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 600, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 600, 0)})
	c.Analysis.Validity.State = result.ValidityInvalid
	rep := mustCompare(t, b, c, Options{})
	found := false
	for _, w := range rep.Warnings {
		found = found || (w.Code == CodeInvalidInput && w.Severity == result.SeverityError)
	}
	if !found {
		t.Fatalf("warnings = %+v", rep.Warnings)
	}
}

func TestSchemaMajorsMustMatch(t *testing.T) {
	b := run(t, "a", tier{name: "http", values: latencies(1, 600, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 600, 0)})
	c.SchemaVersion = "2.0"
	_, err := Compare(b, c, Options{})
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeCompareSchemaMismatch {
		t.Fatalf("err = %v", err)
	}
}

// The interval for p99 over 1..10000 is the order statistics at ranks
// 9900 ± 1.96·√(10000·0.99·0.01), read from the sketch within its accuracy.
func TestQuantileInterval(t *testing.T) {
	vs := make([]float64, 10000)
	for i := range vs {
		vs[i] = float64(i + 1)
	}
	s := sketchOf(t, vs)
	d, err := metrics.DecodeSketch(metrics.EncodedSketch{Encoding: s.Encoding, RelativeAccuracy: s.RelativeAccuracy, Count: s.Count, Data: s.Data})
	if err != nil {
		t.Fatal(err)
	}
	lo, hi, ok := quantileCI(d, 0.99)
	if !ok || math.Abs(lo-9880)/9880 > 0.011 || math.Abs(hi-9920)/9920 > 0.011 {
		t.Fatalf("CI = [%v, %v] ok %v, want about [9880, 9920]", lo, hi, ok)
	}
	small, _ := ddsketch.NewDefaultDDSketch(metrics.DefaultRelativeAccuracy)
	for i := range 50 {
		_ = small.Add(float64(i + 1))
	}
	if _, _, ok := quantileCI(small, 0.99); ok {
		t.Fatal("an interval from 50 samples: rank 49.5+1.4 is past n")
	}
}

func TestIncidentDiff(t *testing.T) {
	inc := func(id string, startS, endS, peak float64, runners ...string) result.Incident {
		i := result.Incident{ID: id, Class: result.IncidentCorrelated, StartS: startS, EndS: endS}
		for _, r := range runners {
			i.Runners = append(i.Runners, result.IncidentRunner{Name: r, Hot: true, PeakP99MS: peak})
		}
		return i
	}
	b := run(t, "a", tier{name: "http", values: latencies(1, 600, 0)}, tier{name: "db", values: latencies(1, 600, 0)})
	c := run(t, "b", tier{name: "http", values: latencies(2, 600, 0)}, tier{name: "db", values: latencies(2, 600, 0)})
	b.Analysis.Incidents = []result.Incident{
		inc("b1", 10, 14, 100, "db"),   // matched, worse later
		inc("b2", 30, 32, 200, "http"), // matched within two buckets, better later
		inc("b3", 50, 52, 80, "db"),    // gone
		inc("b4", 20, 21, 60, "http"),  // same
	}
	c.Analysis.Incidents = []result.Incident{
		inc("c1", 11, 15, 300, "db"),
		inc("c2", 34, 35, 90, "http"),
		inc("c3", 40, 42, 70, "db"), // new
		inc("c4", 20, 21, 62, "http"),
	}
	rep := mustCompare(t, b, c, Options{})
	got := map[string]string{}
	for _, d := range rep.Incidents {
		key := d.Runner + ":"
		if d.Baseline != nil {
			key += d.Baseline.ID
		}
		key += "/"
		if d.Current != nil {
			key += d.Current.ID
		}
		got[key] = d.Status
	}
	want := map[string]string{
		"db:b1/c1": StatusWorsened, "http:b2/c2": StatusImproved, "db:b3/": StatusFixed,
		"db:/c3": StatusNew, "http:b4/c4": StatusUnchanged,
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("incident diff = %v, want %v", got, want)
		}
	}
}

// The rate at which A/A pairs are called changed: requiring two 95% intervals not to
// overlap is conservative, so it should sit well under 1%.
func TestAAFalseChangeRate(t *testing.T) {
	const pairs = 300
	changed := 0
	for seed := range uint64(pairs) {
		b := run(t, "a", tier{name: "http", values: latencies(1000+seed, 3000, 0)})
		c := run(t, "b", tier{name: "http", values: latencies(5000+seed, 3000, 0)})
		if d := delta(mustCompare(t, b, c, Options{}), "http", ""); d.Change != ChangeUnchanged {
			changed++
		}
	}
	t.Logf("%d of %d A/A pairs called changed", changed, pairs)
	if float64(changed)/pairs > 0.01 {
		t.Fatalf("%d of %d A/A pairs were called changed", changed, pairs)
	}
}

// A regression in the tail alone is still a regression, with a note that the
// environment can look the same.
func TestTailOnlyRegressionIsNoted(t *testing.T) {
	base := latencies(1, 6000, 0)
	cur := latencies(2, 6000, 0)
	for i := range 120 { // 2% of requests 200ms slower
		cur[i*50] += 200
	}
	rep := mustCompare(t, run(t, "a", tier{name: "http", values: base}), run(t, "b", tier{name: "http", values: cur}), Options{})
	if !rep.Regression {
		t.Fatalf("summary = %s", rep.Summary)
	}
	found := false
	for _, w := range rep.Warnings {
		found = found || w.Code == CodeTailOnly
	}
	if !found {
		t.Fatalf("warnings = %+v", rep.Warnings)
	}
	shifted := mustCompare(t, run(t, "a", tier{name: "http", values: base}), run(t, "b", tier{name: "http", values: latencies(2, 6000, 50)}), Options{})
	for _, w := range shifted.Warnings {
		if w.Code == CodeTailOnly {
			t.Fatal("a shift of the whole distribution was called tail-only")
		}
	}
}
