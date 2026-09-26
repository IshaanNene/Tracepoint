package capacity

import (
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestJudgeHoldsWithinBudgets(t *testing.T) {
	v := Judge(Level{
		Knob: KnobRate, Value: 100, Runner: "http",
		Measures: []Measure{{Runner: "http", N: 3000, Achieved: 99, P95MS: 80, P99MS: 120, ErrorRatio: 0.001}},
		Budgets:  map[string]Budget{"http": {P99MS: f(250), ErrorRate: f(0.01)}},
	})
	if !v.OK || v.Reason != "" || v.Abort {
		t.Fatalf("verdict = %+v", v)
	}
}

// Any runner's budget breaks a level, not only the knob's: a probe tier breaching its
// own SLO is capacity running out too.
func TestJudgeBreaksOnAnySLO(t *testing.T) {
	v := Judge(Level{
		Knob: KnobRate, Value: 100, Runner: "http",
		Measures: []Measure{
			{Runner: "http", N: 3000, Achieved: 100, P99MS: 120},
			{Runner: "db", N: 1000, Achieved: 30, P99MS: 90, ErrorRatio: 0.2},
		},
		Budgets: map[string]Budget{"http": {P99MS: f(250)}, "db": {P99MS: f(50), ErrorRate: f(0.01)}},
	})
	if v.OK || v.Abort {
		t.Fatalf("verdict = %+v", v)
	}
	for _, want := range []string{"db p99 90ms > 50ms", "db error rate 20.0% > 1.0%"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason %q lacks %q", v.Reason, want)
		}
	}
}

func TestJudgeRatePlateau(t *testing.T) {
	v := Judge(Level{
		Knob: KnobRate, Value: 400, Runner: "http",
		Measures: []Measure{{Runner: "http", N: 9000, Achieved: 340, P99MS: 40}},
	})
	if v.OK || !strings.Contains(v.Reason, "plateau") || !strings.Contains(v.Reason, "85%") {
		t.Fatalf("verdict = %+v", v)
	}
	v = Judge(Level{
		Knob: KnobRate, Value: 400, Runner: "http",
		Measures: []Measure{{Runner: "http", N: 9000, Achieved: 365, P99MS: 40}},
	})
	if !v.OK {
		t.Fatalf("91%% of offered is not a plateau: %+v", v)
	}
}

// For the closed model offered load is an output, so a plateau is more users buying
// almost no more throughput than the level below.
func TestJudgeConcurrencyPlateau(t *testing.T) {
	lower := &Point{N: 32, X: 1000}
	v := Judge(Level{
		Knob: KnobConcurrency, Value: 64, Runner: "http", Below: lower,
		Measures: []Measure{{Runner: "http", N: 30000, Achieved: 1050, P99MS: 40}},
	})
	if v.OK || !strings.Contains(v.Reason, "plateau") {
		t.Fatalf("5%% more throughput for 100%% more users: %+v", v)
	}
	v = Judge(Level{
		Knob: KnobConcurrency, Value: 64, Runner: "http", Below: lower,
		Measures: []Measure{{Runner: "http", N: 30000, Achieved: 1400, P99MS: 40}},
	})
	if !v.OK {
		t.Fatalf("40%% more throughput is scaling: %+v", v)
	}
	v = Judge(Level{
		Knob: KnobConcurrency, Value: 8, Runner: "http",
		Measures: []Measure{{Runner: "http", N: 3000, Achieved: 100, P99MS: 40}},
	})
	if !v.OK {
		t.Fatalf("with nothing below there is nothing to plateau against: %+v", v)
	}
}

func TestJudgeAbortsAboveHalfErrors(t *testing.T) {
	v := Judge(Level{
		Knob: KnobRate, Value: 800, Runner: "http",
		Measures: []Measure{{Runner: "http", N: 9000, Achieved: 790, P99MS: 40, ErrorRatio: 0.6}},
	})
	if v.OK || !v.Abort || !strings.Contains(v.Reason, "60.0%") {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestJudgeWithNothingCompleted(t *testing.T) {
	v := Judge(Level{Knob: KnobRate, Value: 800, Runner: "http", Measures: []Measure{{Runner: "http"}}})
	if v.OK || !strings.Contains(v.Reason, "no http operation completed") {
		t.Fatalf("verdict = %+v", v)
	}
	v = Judge(Level{Knob: KnobRate, Value: 800, Runner: "http"})
	if v.OK {
		t.Fatalf("a level with no measure for its runner held: %+v", v)
	}
}

func TestKnee(t *testing.T) {
	lv := func(v, p99 float64, phase string) Tried {
		return Tried{Value: v, P99MS: p99, Phase: phase}
	}
	levels := []Tried{
		lv(50, 10, PhaseDoubling), lv(100, 11, PhaseDoubling), lv(200, 14, PhaseDoubling),
		lv(400, 90, PhaseDoubling), lv(300, 25, PhaseRefine), lv(300, 400, PhaseConfirm),
	}
	k := Knee(levels)
	if k == nil || *k != 300 {
		t.Fatalf("knee = %v, want 300 (first level past twice the lowest p99; confirmation runs ignored)", k)
	}
	if k := Knee(levels[:3]); k != nil {
		t.Fatalf("knee = %v in flat data", *k)
	}
	if k := Knee(nil); k != nil {
		t.Fatal("knee without levels")
	}
}
