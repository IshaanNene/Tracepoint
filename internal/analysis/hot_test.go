package analysis

import (
	"testing"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// series builds a runner's timeline from service-time p99 values. Every bucket is
// sufficient and outside warm-up unless a test changes it.
func series(p99s ...float64) []result.Bucket {
	out := make([]result.Bucket, len(p99s))
	for i, v := range p99s {
		out[i] = result.Bucket{
			Index: i, OffsetMS: float64(i) * 1000, N: 100,
			Service:  result.Quantiles{P50: v / 2, P99: v},
			Response: result.Quantiles{P50: v / 2, P99: v},
		}
	}
	return out
}

// steady returns n values that wobble around base by a few percent, deterministically,
// so a median absolute deviation exists.
func steady(n int, base float64) []float64 {
	out := make([]float64, n)
	wobble := []float64{0, 0.03, -0.02, 0.05, -0.04, 0.01, -0.01, 0.02}
	for i := range out {
		out[i] = base * (1 + wobble[i%len(wobble)])
	}
	return out
}

func with(vals []float64, extra ...float64) []float64 {
	return append(append([]float64(nil), vals...), extra...)
}

func hotIndexes(h []result.HotBucket) []int {
	out := make([]int, len(h))
	for i, b := range h {
		out[i] = b.Index
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHotAbsoluteOnly(t *testing.T) {
	// Too early for a baseline, but over the threshold: the absolute rule alone fires.
	h := hotBuckets(series(10, 10, 150, 10), 100, DefaultParams())
	if !equalInts(hotIndexes(h), []int{2}) {
		t.Fatalf("hot = %v, want [2]", hotIndexes(h))
	}
	if h[0].Reason != ReasonAbsolute || h[0].P99MS != 150 {
		t.Fatalf("got %+v, want absolute at 150ms", h[0])
	}
}

func TestHotRelativeOnly(t *testing.T) {
	// 60ms is under the 100ms threshold but six times a 10ms baseline.
	vals := with(steady(20, 10), 60)
	h := hotBuckets(series(vals...), 100, DefaultParams())
	if !equalInts(hotIndexes(h), []int{20}) {
		t.Fatalf("hot = %v, want [20]", hotIndexes(h))
	}
	b := h[0]
	if b.Reason != ReasonRelative {
		t.Fatalf("reason = %s, want relative", b.Reason)
	}
	if b.BaselineMS < 9.9 || b.BaselineMS > 10.2 {
		t.Fatalf("baseline = %v, want about 10", b.BaselineMS)
	}
	if b.MZScore <= 3.5 {
		t.Fatalf("mz = %v, want above 3.5", b.MZScore)
	}
}

func TestHotBoth(t *testing.T) {
	h := hotBuckets(series(with(steady(15, 10), 400)...), 100, DefaultParams())
	if len(h) != 1 || h[0].Reason != ReasonBoth {
		t.Fatalf("got %+v, want one bucket hot by both rules", h)
	}
}

// Ten steady buckets are the minimum baseline. With nine, a relative spike is not yet
// judgeable and must not be called.
func TestHotRelativeNeedsTenSteadyBuckets(t *testing.T) {
	if h := hotBuckets(series(with(steady(9, 10), 60)...), 100, DefaultParams()); len(h) != 0 {
		t.Fatalf("hot with 9 baseline buckets: %+v", h)
	}
	if h := hotBuckets(series(with(steady(10, 10), 60)...), 100, DefaultParams()); len(h) != 1 {
		t.Fatalf("not hot with 10 baseline buckets: %+v", h)
	}
}

// The guards exist because a z-score alone fires on noise in a very stable series.
func TestHotRelativeGuards(t *testing.T) {
	cases := []struct {
		name  string
		base  float64
		spike float64
		hot   bool
	}{
		{"huge z, under 3x", 1.0, 2.9, false},
		{"3x but under 5ms above", 2.0, 6.9, false},
		{"3x and 5ms above", 2.0, 7.1, true},
		{"tiny wobble on a flat line", 10, 10.4, false},
	}
	for _, c := range cases {
		h := hotBuckets(series(with(steady(20, c.base), c.spike)...), 100, DefaultParams())
		if got := len(h) == 1; got != c.hot {
			t.Errorf("%s: hot = %v, want %v (%+v)", c.name, got, c.hot, h)
		}
	}
}

// A perfectly flat baseline has a MAD of zero. The floor at the sketch's resolution
// keeps the z-score finite without making a genuine spike invisible.
func TestHotFlatBaseline(t *testing.T) {
	vals := make([]float64, 20)
	for i := range vals {
		vals[i] = 10
	}
	h := hotBuckets(series(with(vals, 50)...), 100, DefaultParams())
	if len(h) != 1 || h[0].Reason != ReasonRelative {
		t.Fatalf("got %+v, want a relative hot bucket", h)
	}
	if h[0].MZScore <= 3.5 || h[0].MZScore > 1e6 {
		t.Fatalf("mz = %v, want finite and above 3.5", h[0].MZScore)
	}
}

// Insufficient and warm-up buckets are never evidence: never hot, and never part of a
// baseline.
func TestHotIgnoresInsufficientAndWarmup(t *testing.T) {
	b := series(with(steady(20, 10), 500, 500)...)
	b[0].Warmup = true
	b[0].Service.P99 = 900
	b[20].Insufficient = true
	b[20].N = 3
	h := hotBuckets(b, 100, DefaultParams())
	if !equalInts(hotIndexes(h), []int{21}) {
		t.Fatalf("hot = %v, want [21]", hotIndexes(h))
	}

	// A baseline polluted by warm-up would sit far higher and hide the spike.
	b = series(with(steady(20, 10), 60)...)
	for i := range 5 {
		b[i].Warmup = true
		b[i].Service.P99 = 1000
	}
	h = hotBuckets(b, 100, DefaultParams())
	if len(h) != 0 {
		// Only 15 steady buckets remain, which is still enough.
		if !equalInts(hotIndexes(h), []int{20}) {
			t.Fatalf("hot = %v, want [20]", hotIndexes(h))
		}
	} else {
		t.Fatalf("the spike was hidden: warm-up leaked into the baseline")
	}
}

// Hot buckets are excluded from later baselines, so a sustained slowdown stays hot for
// its whole length instead of becoming the new normal after a few buckets.
func TestHotSustainedPlateau(t *testing.T) {
	vals := with(steady(20, 10), 60, 60, 60, 60, 60, 60, 60, 60, 60, 60, 60, 60)
	h := hotBuckets(series(vals...), 100, DefaultParams())
	if len(h) != 12 {
		t.Fatalf("hot = %v, want all 12 plateau buckets", hotIndexes(h))
	}
}

// The window is the previous 30 steady buckets, not all of history: an early regime
// the service has long left must not set today's baseline.
func TestHotRollingWindow(t *testing.T) {
	vals := with(steady(40, 50), steady(40, 10)...)
	vals = append(vals, 45)
	h := hotBuckets(series(vals...), 1000, DefaultParams())
	if !equalInts(hotIndexes(h), []int{80}) {
		t.Fatalf("hot = %v, want [80] judged against the recent 10ms regime", hotIndexes(h))
	}
}

func TestThresholds(t *testing.T) {
	budget := config.Duration(250 * time.Millisecond)
	in := Inputs{
		SLO:            config.SLO{HTTP: config.SLOTarget{P99: &budget}},
		FlagThresholds: map[string]float64{"db": 40, "http": 999},
	}
	// §5.5: the SLO p99 first, then the flag, then 100ms.
	got := thresholds([]string{"http", "db", "redis"}, in)
	want := map[string]result.Threshold{
		"http":  {ThresholdMS: 250, Source: SourceSLO},
		"db":    {ThresholdMS: 40, Source: SourceFlag},
		"redis": {ThresholdMS: 100, Source: SourceDefault},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}
}
