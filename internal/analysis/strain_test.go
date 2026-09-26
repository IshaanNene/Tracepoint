package analysis

import (
	"strings"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// rampRun is an app runner whose load grows one step per bucket: in flight i+1 and
// i*10 req/s at bucket i, with service p99 given per bucket.
func rampRun(execType string, p99s ...float64) *result.Result {
	start := 0.0
	b := series(p99s...)
	for i := range b {
		b[i].InFlightMax = int64(i + 1)
		b[i].RPS = float64(i * 10)
	}
	return &result.Result{
		Run: result.Run{BucketMS: 1000},
		Runners: []result.Runner{{
			Name: "http", Kind: "app", Buckets: b,
			Executor: &result.ExecutorStats{Type: execType, StartTarget: &start,
				Stages: []result.Stage{{DurationMS: float64(len(p99s)) * 1000, Target: 100}}},
		}},
	}
}

func TestStrainFoundAtTheFirstSustainedRise(t *testing.T) {
	// Twenty steady buckets at 10ms, a two-bucket blip that is not sustained, then a
	// sustained rise from bucket 30.
	vals := with(steady(20, 10), 10, 10, 10, 10, 30, 30, 10, 10, 10, 10, 25, 26, 40, 60, 80)
	s := findStrain(rampRun("arrival-rate", vals...), DefaultParams())
	if s == nil || !s.Found {
		t.Fatalf("strain = %+v, want found", s)
	}
	if s.AtOffsetS != 30 || s.Users != 31 || s.RPS != 300 {
		t.Fatalf("at %vs, %v users, %v rps; want 30s, 31, 300", s.AtOffsetS, s.Users, s.RPS)
	}
	if s.BaselineP99MS < 9.5 || s.BaselineP99MS > 10.5 {
		t.Fatalf("baseline %v, want about 10", s.BaselineP99MS)
	}
	if !strings.Contains(s.Message, "strain begins at ~31 users (~300 req/s)") {
		t.Fatalf("message %q", s.Message)
	}
	w, ok := s.NextWindow.(map[string]any)
	if !ok || w["knob"] != "rate" || w["from"] != 150.0 || w["to"] != 450.0 {
		t.Fatalf("next window %+v", s.NextWindow)
	}
}

// Rising to exactly twice the baseline is not strain: the rule is strictly above.
func TestStrainNeedsMoreThanDouble(t *testing.T) {
	vals := with(steady(20, 10), 20, 20, 20, 20, 20)
	s := findStrain(rampRun("arrival-rate", vals...), DefaultParams())
	if s == nil || s.Found {
		t.Fatalf("strain = %+v, want not found", s)
	}
	if !strings.Contains(s.Message, "no strain up to ~25 users (~240 req/s)") {
		t.Fatalf("message %q", s.Message)
	}
	if w := s.NextWindow.(map[string]any); w["from"] != 240.0 || w["to"] != 480.0 {
		t.Fatalf("next window %+v", w)
	}
}

// Buckets with too few samples, or in warm-up, are not evidence, and they break a run:
// three sufficient buckets must be consecutive.
func TestStrainSkipsIneligibleBuckets(t *testing.T) {
	r := rampRun("arrival-rate", with(steady(20, 10), 50, 50, 50, 50)...)
	r.Runners[0].Buckets[21].Insufficient = true
	if s := findStrain(r, DefaultParams()); s.Found {
		t.Fatalf("a run broken by a thin bucket counted: %+v", s)
	}
	r = rampRun("arrival-rate", with(steady(20, 10), 50, 50, 50, 50, 50)...)
	r.Runners[0].Buckets[21].Insufficient = true
	if s := findStrain(r, DefaultParams()); !s.Found || s.AtOffsetS != 22 {
		t.Fatalf("strain = %+v, want found at 22s", s)
	}
}

// Under the closed model the knob a follow-up search turns is the user count.
func TestStrainUnderVUsSpeaksOfConcurrency(t *testing.T) {
	s := findStrain(rampRun("vus", with(steady(20, 10), 50, 50, 50)...), DefaultParams())
	if w := s.NextWindow.(map[string]any); w["knob"] != "concurrency" || w["from"] != 10.0 || w["to"] != 32.0 {
		t.Fatalf("next window %+v", w)
	}
}

// A constant load is not a ramp, so there is nothing to find; nor with no app runner.
func TestStrainOnlyOnRamps(t *testing.T) {
	r := rampRun("arrival-rate", with(steady(20, 10), 50, 50, 50)...)
	hold := 100.0
	r.Runners[0].Executor.StartTarget = &hold
	if s := findStrain(r, DefaultParams()); s != nil {
		t.Fatalf("a hold produced %+v", s)
	}
	r.Runners[0].Kind = "storage"
	if s := findStrain(r, DefaultParams()); s != nil {
		t.Fatalf("no app runner produced %+v", s)
	}
}

// Too little steady data to form a baseline is said, not guessed at.
func TestStrainNeedsABaseline(t *testing.T) {
	if s := findStrain(rampRun("arrival-rate", 10, 10, 50, 50, 50), DefaultParams()); s != nil {
		t.Fatalf("five buckets produced %+v", s)
	}
}
