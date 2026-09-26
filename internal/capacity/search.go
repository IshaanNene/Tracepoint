package capacity

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Phases of a search, as recorded against each level.
const (
	PhaseDoubling = "doubling"
	PhaseRefine   = "refine"
	PhaseConfirm  = "confirm"
)

// Plan is what a search needs to know before it starts.
type Plan struct {
	Start float64
	Max   float64
	// Resolution ends refinement once last-ok and first-broken are this close. Zero
	// means max(1, 5% of the first broken level).
	Resolution float64
	// Linear is the number of evenly spaced levels per refinement pass; zero means
	// bisection.
	Linear  int
	Confirm bool
}

// Boundary is where capacity ran out: the highest level that held and the lowest
// that broke, zero for none. Range is set when it is unstable after confirmation.
type Boundary struct {
	LastOK      float64
	FirstBroken float64
	Stable      bool
	Range       []float64
}

// Step is one level to run.
type Step struct {
	Value float64
	Phase string
}

// Search is the level-by-level state of an auto-ramp capacity search (spec §5.6). It
// decides nothing about how a level went: the caller runs each step and records
// whether it held.
//
// Doubling from Start finds the first level that breaks, or reaches Max. Refinement
// then narrows the gap between the last level that held and the first that broke -
// by bisection, or by passes of evenly spaced levels - until it is within the
// resolution. Levels are whole numbers. Confirmation re-runs both sides once; if
// either flips, the boundary is reported as a range.
type Search struct {
	p       Plan
	next    Step
	done    bool
	aborted bool
	// lo is the highest level that held, hi the lowest that broke; zero for none.
	lo, hi   float64
	queue    []float64
	history  map[float64][]bool
	confirms int
}

// NewSearch validates a plan and starts a search.
func NewSearch(p Plan) (*Search, error) {
	bad := func(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }
	switch {
	case bad(p.Start) || p.Start <= 0:
		return nil, errs.New(errs.CodeConfigInvalidValue, "capacity.start must be above zero").WithPath("/capacity/start")
	case bad(p.Max) || p.Max < p.Start:
		return nil, errs.New(errs.CodeConfigInvalidValue, "capacity.max (%g) must be at least capacity.start (%g)", p.Max, p.Start).WithPath("/capacity/max")
	case p.Linear < 0:
		return nil, errs.New(errs.CodeConfigInvalidValue, "capacity.refine linear:N needs N of at least 1").WithPath("/capacity/refine")
	case bad(p.Resolution) || p.Resolution < 0:
		return nil, errs.New(errs.CodeConfigInvalidValue, "capacity.resolution must be above zero").WithPath("/capacity/resolution")
	}
	return &Search{p: p, next: Step{Value: p.Start, Phase: PhaseDoubling}, history: map[float64][]bool{}}, nil
}

// ParseRefine reads capacity.refine: "bisect" (or empty) is 0, "linear:N" is N.
func ParseRefine(s string) (int, error) {
	if s == "" || s == "bisect" {
		return 0, nil
	}
	if n, ok := strings.CutPrefix(s, "linear:"); ok {
		if v, err := strconv.Atoi(n); err == nil && v >= 1 {
			return v, nil
		}
	}
	return 0, errs.New(errs.CodeConfigInvalidValue, "capacity.refine %q is neither bisect nor linear:N with N of at least 1", s).
		WithPath("/capacity/refine")
}

// Next returns the level to run, or false when the search is over.
func (s *Search) Next() (Step, bool) {
	if s.done {
		return Step{}, false
	}
	return s.next, true
}

// Record says whether the level Next returned held.
func (s *Search) Record(ok bool) {
	if s.done {
		return
	}
	v := s.next.Value
	s.history[v] = append(s.history[v], ok)

	switch s.next.Phase {
	case PhaseDoubling:
		if !ok {
			s.hi = v
			s.refine()
			return
		}
		s.lo = v
		if v >= s.p.Max {
			s.done = true // nothing broke up to the ceiling; there is no boundary to confirm
			return
		}
		s.next = Step{Value: math.Min(2*v, s.p.Max), Phase: PhaseDoubling}
	case PhaseRefine:
		if ok {
			s.lo = math.Max(s.lo, v)
		} else {
			s.hi = math.Min(s.hi, v)
			s.queue = nil // a break inside a linear pass ends it
		}
		s.refine()
	case PhaseConfirm:
		s.confirms++
		s.queue = s.queue[1:]
		if len(s.queue) == 0 {
			s.done = true
			return
		}
		s.next = Step{Value: s.queue[0], Phase: PhaseConfirm}
	}
}

// Abort ends the search at once. The level in progress counts as broken.
func (s *Search) Abort() {
	if s.done {
		return
	}
	v := s.next.Value
	s.history[v] = append(s.history[v], false)
	if s.hi == 0 || v < s.hi {
		s.hi = v
	}
	s.done, s.aborted = true, true
}

// Aborted reports whether the search was cut short.
func (s *Search) Aborted() bool { return s.aborted }

func (s *Search) resolution() float64 {
	if s.p.Resolution > 0 {
		return s.p.Resolution
	}
	return math.Max(1, 0.05*s.hi)
}

// refine picks the next refinement level, or moves on to confirmation.
func (s *Search) refine() {
	if s.lo == 0 || s.hi-s.lo <= s.resolution() {
		s.confirm()
		return
	}
	if s.p.Linear > 0 {
		if len(s.queue) == 0 {
			n := float64(s.p.Linear + 1)
			for k := 1; k <= s.p.Linear; k++ {
				x := math.Round(s.lo + (s.hi-s.lo)*float64(k)/n)
				if x > s.lo && x < s.hi && !slices.Contains(s.queue, x) {
					s.queue = append(s.queue, x)
				}
			}
			if len(s.queue) == 0 {
				s.confirm()
				return
			}
		}
		s.next = Step{Value: s.queue[0], Phase: PhaseRefine}
		s.queue = s.queue[1:]
		return
	}
	mid := math.Round((s.lo + s.hi) / 2)
	if mid <= s.lo || mid >= s.hi {
		s.confirm()
		return
	}
	s.next = Step{Value: mid, Phase: PhaseRefine}
}

func (s *Search) confirm() {
	s.queue = nil
	if !s.p.Confirm {
		s.done = true
		return
	}
	if s.lo > 0 {
		s.queue = append(s.queue, s.lo)
	}
	s.queue = append(s.queue, s.hi)
	s.next = Step{Value: s.queue[0], Phase: PhaseConfirm}
}

// Boundary is where capacity ran out. It is unstable when the search was aborted, or
// when a confirmation run flipped - in which case the range is from the highest level
// that held on every run to the lowest that broke on every run.
func (s *Search) Boundary() Boundary {
	b := Boundary{LastOK: s.lo, FirstBroken: s.hi, Stable: !s.aborted}
	if s.confirms == 0 || s.aborted {
		return b
	}
	flipped := false
	if runs := s.history[s.lo]; s.lo > 0 && slices.Contains(runs, false) {
		flipped = true
	}
	if runs := s.history[s.hi]; slices.Contains(runs, true) {
		flipped = true
	}
	if !flipped {
		return b
	}
	b.Stable = false
	upper := s.p.Max
	for v, runs := range s.history {
		if !slices.Contains(runs, true) && v < upper {
			upper = v
		}
	}
	lower := 0.0
	for v, runs := range s.history {
		if !slices.Contains(runs, false) && v > lower && v < upper {
			lower = v
		}
	}
	b.Range = []float64{lower, upper}
	return b
}

// MaxLevels bounds how many levels a plan can take, whatever the target does, for
// planning a search's duration.
func MaxLevels(p Plan) int {
	doubling := 1
	for v := p.Start; v < p.Max; v = math.Min(2*v, p.Max) {
		doubling++
	}
	gap := math.Max(p.Max, 2)
	var refine int
	if p.Linear > 0 {
		refine = p.Linear * (int(math.Ceil(math.Log(gap)/math.Log(float64(p.Linear+1)))) + 2)
	} else {
		refine = int(math.Ceil(math.Log2(gap))) + 2
	}
	confirm := 0
	if p.Confirm {
		confirm = 2
	}
	return doubling + refine + confirm
}
