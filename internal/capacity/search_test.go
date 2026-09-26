package capacity

import (
	"math"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// drive runs a search against a system whose answer at each value comes from ok, and
// returns every step taken.
// fataler is what drive needs from a *testing.T or a *rapid.T.
type fataler interface{ Fatal(args ...any) }

func drive(t fataler, s *Search, ok func(v float64, run int) bool) []Step {
	var steps []Step
	runs := map[float64]int{}
	for {
		st, more := s.Next()
		if !more {
			return steps
		}
		steps = append(steps, st)
		runs[st.Value]++
		s.Record(ok(st.Value, runs[st.Value]))
		if len(steps) > 1000 {
			t.Fatal("the search did not terminate")
		}
	}
}

func below(b float64) func(float64, int) bool {
	return func(v float64, _ int) bool { return v < b }
}

func values(steps []Step, phase string) []float64 {
	var out []float64
	for _, s := range steps {
		if phase == "" || s.Phase == phase {
			out = append(out, s.Value)
		}
	}
	return out
}

func mustSearch(t testing.TB, p Plan) *Search {
	t.Helper()
	s, err := NewSearch(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSearchDoublesThenBisectsThenConfirms(t *testing.T) {
	s := mustSearch(t, Plan{Start: 50, Max: 2000, Confirm: true})
	steps := drive(t, s, below(300))

	if got := values(steps, PhaseDoubling); !slices.Equal(got, []float64{50, 100, 200, 400}) {
		t.Fatalf("doubling = %v", got)
	}
	refine := values(steps, PhaseRefine)
	if len(refine) == 0 || refine[0] != 300 {
		t.Fatalf("refine = %v, want bisection starting at 300", refine)
	}
	b := s.Boundary()
	if b.LastOK >= 300 || b.FirstBroken < 300 || !b.Stable || b.Range != nil {
		t.Fatalf("boundary = %+v", b)
	}
	// Refinement stops once the gap is within 5% of the broken level.
	if gap := b.FirstBroken - b.LastOK; gap > math.Max(1, 0.05*b.FirstBroken) {
		t.Fatalf("gap %v is above the resolution", gap)
	}
	if got := values(steps, PhaseConfirm); !slices.Equal(got, []float64{b.LastOK, b.FirstBroken}) {
		t.Fatalf("confirm = %v, boundary %+v", got, b)
	}
	for _, st := range steps {
		if st.Value != math.Round(st.Value) {
			t.Fatalf("a fractional level: %v", st.Value)
		}
	}
}

func TestSearchHonoursAnExplicitResolution(t *testing.T) {
	s := mustSearch(t, Plan{Start: 50, Max: 2000, Resolution: 40})
	drive(t, s, below(300))
	b := s.Boundary()
	if gap := b.FirstBroken - b.LastOK; gap > 40 || gap <= 20 {
		t.Fatalf("gap %v with resolution 40: bisection should stop as soon as it is within it", gap)
	}
}

func TestSearchCapsAtMax(t *testing.T) {
	s := mustSearch(t, Plan{Start: 50, Max: 1000, Confirm: true})
	steps := drive(t, s, below(math.Inf(1)))
	if got := values(steps, ""); !slices.Equal(got, []float64{50, 100, 200, 400, 800, 1000}) {
		t.Fatalf("levels = %v", got)
	}
	b := s.Boundary()
	if b.LastOK != 1000 || b.FirstBroken != 0 || !b.Stable {
		t.Fatalf("boundary = %+v", b)
	}
}

func TestSearchWhenTheFirstLevelBreaks(t *testing.T) {
	s := mustSearch(t, Plan{Start: 50, Max: 1000, Confirm: true})
	steps := drive(t, s, below(10))
	if got := values(steps, ""); !slices.Equal(got, []float64{50, 50}) {
		t.Fatalf("levels = %v: the start is re-run to confirm and nothing is refined below it", got)
	}
	b := s.Boundary()
	if b.LastOK != 0 || b.FirstBroken != 50 || !b.Stable {
		t.Fatalf("boundary = %+v", b)
	}
}

func TestSearchLinearFill(t *testing.T) {
	s := mustSearch(t, Plan{Start: 100, Max: 1000, Linear: 3})
	steps := drive(t, s, below(330))
	refine := values(steps, PhaseRefine)
	// Between 200 (ok) and 400 (broken): 250, 300, 350 - and 350 breaks, ending the pass.
	if len(refine) < 3 || !slices.Equal(refine[:3], []float64{250, 300, 350}) {
		t.Fatalf("refine = %v", refine)
	}
	b := s.Boundary()
	if b.LastOK >= 330 || b.FirstBroken < 330 || b.FirstBroken-b.LastOK > math.Max(1, 0.05*b.FirstBroken) {
		t.Fatalf("boundary = %+v", b)
	}
}

func TestSearchWithoutConfirmation(t *testing.T) {
	s := mustSearch(t, Plan{Start: 50, Max: 2000})
	steps := drive(t, s, below(300))
	if got := values(steps, PhaseConfirm); len(got) != 0 {
		t.Fatalf("confirmation ran: %v", got)
	}
	if !s.Boundary().Stable {
		t.Fatal("an unconfirmed boundary is reported as unstable")
	}
}

// When a re-run flips, the boundary is a range, not a point.
func TestSearchReportsAnUnstableRange(t *testing.T) {
	s := mustSearch(t, Plan{Start: 100, Max: 1000, Resolution: 60, Confirm: true})
	var lastOK float64
	steps := drive(t, s, func(v float64, run int) bool {
		if run > 1 && v < 300 {
			return false // the last ok level breaks on its re-run
		}
		return v < 300
	})
	b := s.Boundary()
	lastOK = values(steps, PhaseConfirm)[0]
	if b.Stable || len(b.Range) != 2 {
		t.Fatalf("boundary = %+v", b)
	}
	if b.Range[0] >= lastOK || b.Range[1] != b.FirstBroken {
		t.Fatalf("range %v: want below the flipped level %v, up to %v", b.Range, lastOK, b.FirstBroken)
	}
	// The lower end held on every run.
	for _, st := range steps {
		if st.Value == b.Range[0] && st.Value >= 300 {
			t.Fatalf("range lower end %v never held", b.Range[0])
		}
	}
}

func TestSearchAbortsAtOnce(t *testing.T) {
	s := mustSearch(t, Plan{Start: 50, Max: 2000, Confirm: true})
	n := 0
	for {
		st, more := s.Next()
		if !more {
			break
		}
		n++
		if st.Value == 200 {
			s.Abort()
			continue
		}
		s.Record(true)
	}
	if n != 3 || !s.Aborted() {
		t.Fatalf("%d levels, aborted %v", n, s.Aborted())
	}
	b := s.Boundary()
	if b.Stable || b.LastOK != 100 || b.FirstBroken != 200 {
		t.Fatalf("boundary = %+v", b)
	}
	if _, more := s.Next(); more {
		t.Fatal("a level after abort")
	}
}

func TestPlanValidation(t *testing.T) {
	for _, p := range []Plan{
		{Start: 0, Max: 10},
		{Start: 10, Max: 5},
		{Start: 1, Max: 10, Linear: -1},
		{Start: 1, Max: 10, Resolution: -2},
		{Start: math.NaN(), Max: 10},
	} {
		if _, err := NewSearch(p); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
}

func TestParseRefine(t *testing.T) {
	for in, want := range map[string]int{"": 0, "bisect": 0, "linear:4": 4} {
		got, err := ParseRefine(in)
		if err != nil || got != want {
			t.Errorf("ParseRefine(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"linear:0", "linear:x", "binary"} {
		if _, err := ParseRefine(bad); err == nil {
			t.Errorf("ParseRefine(%q) accepted", bad)
		}
	}
}

// Property: whatever the system, the search terminates within MaxLevels, every level
// lies within [start, max], and the boundary brackets the true break.
func TestSearchIsBoundedProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		start := float64(rapid.IntRange(1, 200).Draw(rt, "start"))
		maxV := start * float64(rapid.IntRange(1, 64).Draw(rt, "factor"))
		p := Plan{
			Start: start, Max: maxV,
			Linear:  rapid.IntRange(0, 5).Draw(rt, "linear"),
			Confirm: rapid.Bool().Draw(rt, "confirm"),
		}
		b := rapid.Float64Range(0.5, maxV*1.5).Draw(rt, "boundary")
		s, err := NewSearch(p)
		if err != nil {
			rt.Fatal(err)
		}
		steps := drive(rt, s, below(b))
		if len(steps) > MaxLevels(p) {
			rt.Fatalf("%d levels, bound %d", len(steps), MaxLevels(p))
		}
		for _, st := range steps {
			if st.Value < start || st.Value > maxV {
				rt.Fatalf("level %v outside [%v, %v]", st.Value, start, maxV)
			}
		}
		got := s.Boundary()
		if got.LastOK != 0 && got.LastOK >= b {
			rt.Fatalf("last ok %v is not below the break %v", got.LastOK, b)
		}
		if got.FirstBroken != 0 && got.FirstBroken < b {
			rt.Fatalf("first broken %v is below the break %v", got.FirstBroken, b)
		}
		if got.FirstBroken != 0 && got.LastOK != 0 {
			if gap := got.FirstBroken - got.LastOK; gap > math.Max(1, 0.05*got.FirstBroken)+1e-9 {
				rt.Fatalf("gap %v above resolution (boundary %+v)", gap, got)
			}
		}
	})
}
