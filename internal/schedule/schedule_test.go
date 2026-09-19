package schedule_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"go.uber.org/goleak"
	"pgregory.net/rapid"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/schedule"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func newRNG() *rand.Rand { return rand.New(rand.NewPCG(42, 1024)) }

func mustProfile(t *testing.T, start float64, stages ...schedule.Stage) *schedule.Profile {
	t.Helper()
	p, err := schedule.NewProfile(start, stages)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	return p
}

// The standing sanity check from ADR-001: a linear ramp from zero to R over T offers
// exactly R*T/2 arrivals, because that is the area under the ramp.
func TestRampOffersHalfTheRectangle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rate float64
		over time.Duration
	}{
		{200, 30 * time.Second},
		{500, time.Minute},
		{1000, 12500 * time.Millisecond},
		{0.5, 10 * time.Second},
	}
	for _, c := range cases {
		p := mustProfile(t, 0, schedule.Stage{Duration: c.over, Target: c.rate})
		want := c.rate * c.over.Seconds() / 2
		if got := p.Expected(); math.Abs(got-want) > 1e-9 {
			t.Errorf("ramp to %v over %v: expected %v arrivals, want %v", c.rate, c.over, got, want)
		}
	}
}

// A constant stage - what the `rate:` shorthand compiles to - must hold its rate
// rather than ramping up to it from zero.
func TestConstantStageHoldsItsRate(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 40, schedule.Stage{Duration: 10 * time.Second, Target: 40})
	if got := p.Expected(); math.Abs(got-400) > 1e-9 {
		t.Errorf("40/s for 10s offered %v arrivals, want 400", got)
	}
	for _, at := range []time.Duration{0, 3 * time.Second, 10 * time.Second} {
		if got := p.RateAt(at); math.Abs(got-40) > 1e-9 {
			t.Errorf("RateAt(%v) = %v, want a flat 40", at, got)
		}
	}
}

// Inverting the cumulative arrival count at the end of a stage must land exactly on
// the stage boundary, for every stage shape. This is the identity that catches an
// algebra slip in the quadratic root.
func TestInvertAtStageBoundaryIsExact(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		start float64
		stage schedule.Stage
	}{
		{"ramp up", 0, schedule.Stage{Duration: 30 * time.Second, Target: 200}},
		{"hold", 200, schedule.Stage{Duration: 150 * time.Second, Target: 200}},
		{"ramp down", 500, schedule.Stage{Duration: 20 * time.Second, Target: 50}},
		{"fractional hold", 0.5, schedule.Stage{Duration: 10 * time.Second, Target: 0.5}},
		{"ramp to zero", 200, schedule.Stage{Duration: 30 * time.Second, Target: 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := mustProfile(t, c.start, c.stage)
			at, ok := p.Invert(p.Expected())
			if !ok {
				t.Fatalf("Invert(total) reported the profile exhausted")
			}
			if diff := (at - c.stage.Duration).Abs(); diff > time.Microsecond {
				t.Errorf("Invert(total) = %v, want %v (off by %v)", at, c.stage.Duration, diff)
			}
		})
	}
}

// The numerically stable root exists because the textbook one loses its significant
// digits as the ramp flattens. Round-tripping Lambda through Invert is how we detect
// that: a near-constant stage is the worst case.
func TestInvertIsStableOnANearConstantStage(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 200, schedule.Stage{Duration: 150 * time.Second, Target: 200.000001})
	for _, k := range []float64{1, 100, 10_000, 29_999} {
		at, ok := p.Invert(k)
		if !ok {
			t.Fatalf("Invert(%v) reported the profile exhausted", k)
		}
		if back := p.Lambda(at); math.Abs(back-k) > 1e-6 {
			t.Errorf("Lambda(Invert(%v)) = %v, off by %g", k, back, back-k)
		}
	}
}

func TestMultiStageProfile(t *testing.T) {
	t.Parallel()
	// Ramp 0->200 over 30s, hold 150s, ramp 200->0 over 30s.
	p := mustProfile(t, 0,
		schedule.Stage{Duration: 30 * time.Second, Target: 200},
		schedule.Stage{Duration: 150 * time.Second, Target: 200},
		schedule.Stage{Duration: 30 * time.Second, Target: 0},
	)
	if got, want := p.Duration(), 210*time.Second; got != want {
		t.Errorf("Duration() = %v, want %v", got, want)
	}
	// 3000 on the way up + 30000 holding + 3000 on the way down.
	if got := p.Expected(); math.Abs(got-36_000) > 1e-6 {
		t.Errorf("Expected() = %v, want 36000", got)
	}
	for _, c := range []struct {
		at   time.Duration
		rate float64
	}{
		{0, 0}, {15 * time.Second, 100}, {30 * time.Second, 200},
		{100 * time.Second, 200}, {195 * time.Second, 100}, {210 * time.Second, 0},
	} {
		if got := p.RateAt(c.at); math.Abs(got-c.rate) > 1e-9 {
			t.Errorf("RateAt(%v) = %v, want %v", c.at, got, c.rate)
		}
	}
}

// A stage that offers nothing must be skipped rather than dividing by zero.
func TestZeroRateStageIsSkipped(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 0,
		schedule.Stage{Duration: 10 * time.Second, Target: 0},   // silent
		schedule.Stage{Duration: 10 * time.Second, Target: 100}, // ramp up afterwards
	)
	if got := p.Expected(); math.Abs(got-500) > 1e-9 {
		t.Errorf("Expected() = %v, want 500", got)
	}
	at, ok := p.Invert(1)
	if !ok {
		t.Fatal("Invert(1) reported the profile exhausted")
	}
	if at < 10*time.Second {
		t.Errorf("first arrival at %v, but the first 10s offer no load", at)
	}
}

func TestInvertBeyondTheProfileReportsExhausted(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 10, schedule.Stage{Duration: time.Second, Target: 10})
	if _, ok := p.Invert(p.Expected() + 0.5); ok {
		t.Error("Invert past the end should report the profile exhausted")
	}
}

func TestUniformScheduleMatchesTheProfile(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 100, schedule.Stage{Duration: 10 * time.Second, Target: 100})
	s, err := schedule.NewSchedule(p, schedule.ArrivalUniform, newRNG())
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	var offsets []time.Duration
	for {
		at, ok := s.Next()
		if !ok {
			break
		}
		offsets = append(offsets, at)
	}
	if got, want := len(offsets), 1000; got != want {
		t.Fatalf("got %d arrivals, want %d", got, want)
	}
	// Uniform arrivals on a flat profile are evenly spaced: the kth at k/rate.
	for i, at := range offsets {
		want := time.Duration(float64(i+1) / 100 * float64(time.Second))
		if diff := (at - want).Abs(); diff > time.Microsecond {
			t.Fatalf("arrival %d at %v, want %v", i+1, at, want)
		}
	}
}

// Poisson feeds cumulative Exp(1) sums into the same inverse (time rescaling), so the
// mean rate is unchanged while the spacing becomes irregular.
func TestPoissonHasTheSameMeanRateButIrregularSpacing(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 100, schedule.Stage{Duration: 100 * time.Second, Target: 100})
	s, err := schedule.NewSchedule(p, schedule.ArrivalPoisson, newRNG())
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	var offsets []time.Duration
	for {
		at, ok := s.Next()
		if !ok {
			break
		}
		offsets = append(offsets, at)
	}
	// 10,000 expected; Poisson sampling means the realised count varies, but not by
	// much over this many arrivals.
	if len(offsets) < 9_700 || len(offsets) > 10_300 {
		t.Errorf("got %d arrivals, want roughly 10000", len(offsets))
	}
	// Irregular: a flat uniform schedule has identical gaps, a Poisson one does not.
	var distinct int
	seen := map[time.Duration]bool{}
	for i := 1; i < len(offsets) && i < 200; i++ {
		gap := (offsets[i] - offsets[i-1]).Round(time.Microsecond)
		if !seen[gap] {
			seen[gap] = true
			distinct++
		}
	}
	if distinct < 50 {
		t.Errorf("only %d distinct gaps in the first 200 arrivals; this looks uniform, not Poisson", distinct)
	}
}

func TestScheduleIsReproducibleFromItsSeed(t *testing.T) {
	t.Parallel()
	collect := func() []time.Duration {
		p := mustProfile(t, 50, schedule.Stage{Duration: 5 * time.Second, Target: 50})
		s, err := schedule.NewSchedule(p, schedule.ArrivalPoisson, rand.New(rand.NewPCG(7, 7)))
		if err != nil {
			t.Fatalf("NewSchedule: %v", err)
		}
		var out []time.Duration
		for {
			at, ok := s.Next()
			if !ok {
				return out
			}
			out = append(out, at)
		}
	}
	a, b := collect(), collect()
	if len(a) != len(b) {
		t.Fatalf("same seed produced %d and %d arrivals", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed diverged at arrival %d: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestInvalidProfilesAreRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		start  float64
		stages []schedule.Stage
	}{
		{"no stages", 10, nil},
		{"negative duration", 10, []schedule.Stage{{Duration: -time.Second, Target: 10}}},
		{"zero duration", 10, []schedule.Stage{{Duration: 0, Target: 10}}},
		{"negative target", 10, []schedule.Stage{{Duration: time.Second, Target: -1}}},
		{"negative start rate", -1, []schedule.Stage{{Duration: time.Second, Target: 10}}},
		{"not a number", math.NaN(), []schedule.Stage{{Duration: time.Second, Target: 10}}},
		{"infinite target", 0, []schedule.Stage{{Duration: time.Second, Target: math.Inf(1)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := schedule.NewProfile(c.start, c.stages); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// Property: for any profile, the number of uniform arrivals equals floor of the
// integral of the rate, within one - the rounding of the last partial arrival.
func TestPropertyArrivalCountMatchesTheIntegral(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		start := rapid.Float64Range(0, 500).Draw(rt, "startRate")
		n := rapid.IntRange(1, 5).Draw(rt, "stages")
		stages := make([]schedule.Stage, n)
		for i := range stages {
			stages[i] = schedule.Stage{
				Duration: time.Duration(rapid.IntRange(1, 60).Draw(rt, "seconds")) * time.Second,
				Target:   rapid.Float64Range(0, 500).Draw(rt, "target"),
			}
		}
		p, err := schedule.NewProfile(start, stages)
		if err != nil {
			rt.Fatalf("NewProfile: %v", err)
		}
		s, err := schedule.NewSchedule(p, schedule.ArrivalUniform, newRNG())
		if err != nil {
			rt.Fatalf("NewSchedule: %v", err)
		}
		count := 0
		for {
			if _, ok := s.Next(); !ok {
				break
			}
			count++
		}
		want := int(math.Floor(p.Expected()))
		if count != want {
			rt.Fatalf("%d arrivals, want %d (integral %v)", count, want, p.Expected())
		}
	})
}

// Property: arrival offsets are strictly increasing and stay inside the profile.
func TestPropertyArrivalsAreMonotonicAndBounded(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		start := rapid.Float64Range(0, 200).Draw(rt, "startRate")
		n := rapid.IntRange(1, 4).Draw(rt, "stages")
		stages := make([]schedule.Stage, n)
		for i := range stages {
			stages[i] = schedule.Stage{
				Duration: time.Duration(rapid.IntRange(1, 30).Draw(rt, "seconds")) * time.Second,
				Target:   rapid.Float64Range(0, 200).Draw(rt, "target"),
			}
		}
		p, err := schedule.NewProfile(start, stages)
		if err != nil {
			rt.Fatalf("NewProfile: %v", err)
		}
		arrival := schedule.ArrivalUniform
		if rapid.Bool().Draw(rt, "poisson") {
			arrival = schedule.ArrivalPoisson
		}
		s, err := schedule.NewSchedule(p, arrival, newRNG())
		if err != nil {
			rt.Fatalf("NewSchedule: %v", err)
		}
		prev := time.Duration(-1)
		for {
			at, ok := s.Next()
			if !ok {
				break
			}
			if at < prev {
				rt.Fatalf("arrival at %v went backwards from %v", at, prev)
			}
			if at < 0 || at > p.Duration() {
				rt.Fatalf("arrival at %v is outside the profile [0, %v]", at, p.Duration())
			}
			prev = at
		}
	})
}

// Property: Lambda is monotonically non-decreasing, since a rate is never negative.
func TestPropertyLambdaIsMonotonic(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		start := rapid.Float64Range(0, 300).Draw(rt, "startRate")
		n := rapid.IntRange(1, 4).Draw(rt, "stages")
		stages := make([]schedule.Stage, n)
		for i := range stages {
			stages[i] = schedule.Stage{
				Duration: time.Duration(rapid.IntRange(1, 30).Draw(rt, "seconds")) * time.Second,
				Target:   rapid.Float64Range(0, 300).Draw(rt, "target"),
			}
		}
		p, err := schedule.NewProfile(start, stages)
		if err != nil {
			rt.Fatalf("NewProfile: %v", err)
		}
		prev := 0.0
		step := p.Duration() / 97
		for at := time.Duration(0); at <= p.Duration(); at += step {
			got := p.Lambda(at)
			if got < prev-1e-9 {
				rt.Fatalf("Lambda(%v) = %v fell below the previous %v", at, got, prev)
			}
			prev = got
		}
	})
}

func BenchmarkScheduleNext(b *testing.B) {
	p, err := schedule.NewProfile(0, []schedule.Stage{
		{Duration: 30 * time.Second, Target: 2000},
		{Duration: 600 * time.Second, Target: 2000},
	})
	if err != nil {
		b.Fatalf("NewProfile: %v", err)
	}
	for b.Loop() {
		s, err := schedule.NewSchedule(p, schedule.ArrivalUniform, newRNG())
		if err != nil {
			b.Fatalf("NewSchedule: %v", err)
		}
		for range 10_000 {
			if _, ok := s.Next(); !ok {
				break
			}
		}
	}
}

func TestProfileEdges(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 100, schedule.Stage{Duration: 10 * time.Second, Target: 100})

	t.Run("rate outside the profile is zero", func(t *testing.T) {
		t.Parallel()
		if got := p.RateAt(-time.Second); got != 0 {
			t.Errorf("RateAt(-1s) = %v, want 0", got)
		}
		if got := p.RateAt(11 * time.Second); got != 0 {
			t.Errorf("RateAt(past the end) = %v, want 0; no load is offered after the profile", got)
		}
		if got := p.RateAt(10 * time.Second); got != 100 {
			t.Errorf("RateAt(exactly the end) = %v, want the final target 100", got)
		}
	})

	t.Run("lambda is clamped at both ends", func(t *testing.T) {
		t.Parallel()
		if got := p.Lambda(-time.Second); got != 0 {
			t.Errorf("Lambda(before the start) = %v, want 0", got)
		}
		if got := p.Lambda(time.Hour); got != p.Expected() {
			t.Errorf("Lambda(past the end) = %v, want the total %v", got, p.Expected())
		}
	})

	t.Run("inverting a non-positive count gives the start", func(t *testing.T) {
		t.Parallel()
		for _, k := range []float64{0, -1} {
			at, ok := p.Invert(k)
			if !ok || at != 0 {
				t.Errorf("Invert(%v) = (%v, %v), want (0, true)", k, at, ok)
			}
		}
	})
}

func TestScheduleConstructionErrors(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 10, schedule.Stage{Duration: time.Second, Target: 10})

	t.Run("nil profile", func(t *testing.T) {
		t.Parallel()
		if _, err := schedule.NewSchedule(nil, schedule.ArrivalUniform, newRNG()); err == nil {
			t.Error("expected an error for a nil profile")
		}
	})
	t.Run("unknown arrival process", func(t *testing.T) {
		t.Parallel()
		if _, err := schedule.NewSchedule(p, schedule.Arrival("gaussian"), newRNG()); err == nil {
			t.Error("expected an error for an unknown arrival process")
		}
	})
	t.Run("poisson without a random source", func(t *testing.T) {
		t.Parallel()
		if _, err := schedule.NewSchedule(p, schedule.ArrivalPoisson, nil); err == nil {
			t.Error("poisson arrivals need a seeded source; expected an error")
		}
	})
	t.Run("uniform needs no random source", func(t *testing.T) {
		t.Parallel()
		if _, err := schedule.NewSchedule(p, schedule.ArrivalUniform, nil); err != nil {
			t.Errorf("uniform arrivals should not require a source: %v", err)
		}
	})
}

func TestScheduleStaysExhausted(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 10, schedule.Stage{Duration: time.Second, Target: 10})
	s, err := schedule.NewSchedule(p, schedule.ArrivalUniform, nil)
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	for {
		if _, ok := s.Next(); !ok {
			break
		}
	}
	if got := s.Count(); got != 10 {
		t.Errorf("Count() = %d, want 10", got)
	}
	// Calling past the end must stay exhausted rather than restarting.
	for range 3 {
		if _, ok := s.Next(); ok {
			t.Fatal("an exhausted schedule produced another arrival")
		}
	}
	if got := s.Count(); got != 10 {
		t.Errorf("Count() = %d after exhaustion, want 10", got)
	}
}

// A profile that offers nothing at all is valid - it is what a zero-rate stage
// compiles to - and must produce no arrivals rather than dividing by zero.
func TestSilentProfileProducesNothing(t *testing.T) {
	t.Parallel()
	p := mustProfile(t, 0, schedule.Stage{Duration: 10 * time.Second, Target: 0})
	if got := p.Expected(); got != 0 {
		t.Errorf("Expected() = %v, want 0", got)
	}
	s, err := schedule.NewSchedule(p, schedule.ArrivalUniform, nil)
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	if at, ok := s.Next(); ok {
		t.Errorf("a silent profile produced an arrival at %v", at)
	}
	if got := p.RateAt(5 * time.Second); got != 0 {
		t.Errorf("RateAt = %v, want 0", got)
	}
}

func TestStagePathIsReportedOnInvalidStages(t *testing.T) {
	t.Parallel()
	_, err := schedule.NewProfile(0, []schedule.Stage{
		{Duration: time.Second, Target: 10},
		{Duration: time.Second, Target: -5},
	})
	if err == nil {
		t.Fatal("expected an error for a negative target")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) {
		t.Fatalf("error is not a coded error: %v", err)
	}
	if typed.Path != "/executor/stages/1" {
		t.Errorf("path = %q, want the offending stage /executor/stages/1", typed.Path)
	}
}
