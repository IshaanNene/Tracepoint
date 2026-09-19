// Package schedule turns a piecewise-linear rate profile into exact arrival times.
//
// The load profile is a sequence of stages; within a stage the rate moves linearly
// from the previous stage's target to this one's. The cumulative expected number of
// arrivals is Lambda(t), the integral of the rate, and arrival k fires at the time
// where Lambda reaches k. Computing that inverse in closed form - rather than
// sleeping 1/rate between arrivals - is what keeps a long run from drifting off its
// nominal rate, and it is why a ramp actually reaches its target.
//
// Both arrival processes share one inverse. Uniform feeds the integers 1, 2, 3 into
// it. Poisson feeds cumulative Exp(1) sums into it, which by the time-rescaling
// theorem yields a non-homogeneous Poisson process with exactly the configured
// intensity: same mean rate, irregular spacing.
//
// See docs/adr/001-in-house-scheduler.md for the derivation and the numerical
// verification behind the root used below.
package schedule

import (
	"math"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Arrival selects the arrival process.
type Arrival string

const (
	// ArrivalUniform spaces arrivals evenly along the rate profile. Easier to reason
	// about when reading a chart, and the default.
	ArrivalUniform Arrival = "uniform"
	// ArrivalPoisson draws exponential gaps with the same mean rate. A better model of
	// independent users, and the bursts it produces are what find queueing problems.
	ArrivalPoisson Arrival = "poisson"
)

// Stage is one segment of a load profile: the rate ramps linearly to Target over
// Duration, starting from the previous stage's target.
type Stage struct {
	Duration time.Duration
	Target   float64
}

// segment is a precompiled stage. Holding the quadratic coefficients and the running
// cumulative total means inverting an arrival is a binary search plus a square root,
// with no work proportional to the arrival index.
type segment struct {
	startOffset float64 // seconds from the run start
	d           float64 // seconds
	r0, r1      float64 // rate at the start and end of the segment

	// Lambda(s) = a*s^2 + b*s within the segment, for local time s in [0, d].
	a, b float64

	cumStart float64 // Lambda at the segment start
	capacity float64 // arrivals offered within this segment
}

// Profile is a compiled, piecewise-linear rate profile.
type Profile struct {
	segs     []segment
	duration time.Duration
	total    float64
}

// NewProfile compiles a rate profile that begins at startRate and follows stages.
//
// startRate is what makes the two configuration forms behave differently, as they
// should: the `rate: 40` shorthand compiles to NewProfile(40, [{d, 40}]) and holds a
// flat 40, while an explicit `stages:` list compiles to NewProfile(0, stages) and
// ramps up from nothing.
func NewProfile(startRate float64, stages []Stage) (*Profile, error) {
	if len(stages) == 0 {
		return nil, errs.New(errs.CodeConfigInvalidValue, "a load profile needs at least one stage").
			WithHint("set executor.rate for a constant rate, or executor.stages for a ramp")
	}
	if err := finite("the starting rate", startRate); err != nil {
		return nil, err
	}
	if startRate < 0 {
		return nil, errs.New(errs.CodeConfigInvalidValue, "the starting rate must not be negative, got %v", startRate)
	}

	p := &Profile{segs: make([]segment, 0, len(stages))}
	prev := startRate
	var offset float64
	for i, st := range stages {
		if st.Duration <= 0 {
			return nil, errs.New(errs.CodeConfigInvalidValue, "stage %d has a duration of %v", i, st.Duration).
				WithPath(stagePath(i)).
				WithHint("every stage needs a positive duration, for example 30s")
		}
		if err := finite("stage target", st.Target); err != nil {
			return nil, err.WithPath(stagePath(i))
		}
		if st.Target < 0 {
			return nil, errs.New(errs.CodeConfigInvalidValue, "stage %d has a negative target of %v", i, st.Target).
				WithPath(stagePath(i))
		}

		d := st.Duration.Seconds()
		seg := segment{
			startOffset: offset,
			d:           d,
			r0:          prev,
			r1:          st.Target,
			a:           (st.Target - prev) / (2 * d),
			b:           prev,
			cumStart:    p.total,
		}
		seg.capacity = seg.a*d*d + seg.b*d

		p.segs = append(p.segs, seg)
		// Accumulate through the same path Invert walks, so Expected and Invert agree
		// bit for bit and a caller cannot ask for an arrival the profile claims to have.
		p.total += seg.capacity
		p.duration += st.Duration
		offset += d
		prev = st.Target
	}
	return p, nil
}

// Duration is the total length of the profile.
func (p *Profile) Duration() time.Duration { return p.duration }

// Expected is the total number of arrivals the profile offers: Lambda at the end.
func (p *Profile) Expected() float64 { return p.total }

// RateAt returns the instantaneous offered rate at an offset from the run start.
// Beyond the end of the profile the rate is zero: no more load is offered.
func (p *Profile) RateAt(at time.Duration) float64 {
	switch {
	case at < 0:
		return 0
	case at > p.duration:
		return 0
	}
	t := at.Seconds()
	seg := p.segmentAtTime(t)
	s := t - seg.startOffset
	if s > seg.d {
		s = seg.d
	}
	return seg.r0 + (seg.r1-seg.r0)*(s/seg.d)
}

// Lambda returns the cumulative expected arrivals by an offset from the run start.
func (p *Profile) Lambda(at time.Duration) float64 {
	switch {
	case at <= 0:
		return 0
	case at >= p.duration:
		return p.total
	}
	t := at.Seconds()
	seg := p.segmentAtTime(t)
	s := t - seg.startOffset
	return seg.cumStart + seg.a*s*s + seg.b*s
}

// Invert returns the offset at which the kth arrival fires, reporting false once k is
// beyond what the profile offers.
func (p *Profile) Invert(k float64) (time.Duration, bool) {
	if k <= 0 {
		return 0, true
	}
	if k > p.total {
		return 0, false
	}
	seg := p.segmentAtCount(k)
	s := solve(seg.a, seg.b, k-seg.cumStart)
	// Guard against a root that floating point nudges just outside the segment.
	if s < 0 {
		s = 0
	}
	if s > seg.d {
		s = seg.d
	}
	return seconds(seg.startOffset + s), true
}

// segmentAtTime finds the segment containing an offset in seconds.
func (p *Profile) segmentAtTime(t float64) segment {
	lo, hi := 0, len(p.segs)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if p.segs[mid].startOffset <= t {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return p.segs[lo]
}

// segmentAtCount finds the segment that the kth arrival falls in. Segments offering
// nothing - a stage held at zero - have zero capacity and are stepped over here, which
// is what keeps solve from being handed a degenerate quadratic.
func (p *Profile) segmentAtCount(k float64) segment {
	lo, hi := 0, len(p.segs)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if p.segs[mid].cumStart < k {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	// Step forward over any empty segments so the caller never solves inside one.
	for lo < len(p.segs)-1 && p.segs[lo].capacity == 0 {
		lo++
	}
	return p.segs[lo]
}

// solve returns the non-negative root of a*s^2 + b*s = c.
//
// The textbook root (-b + sqrt(b^2+4ac)) / 2a subtracts two nearly equal numbers and
// divides by something tiny whenever a is small - which is the common case, since a is
// zero for a constant rate. Rationalising the numerator gives an algebraically
// identical expression that is well conditioned everywhere, and that needs no special
// case for a == 0: it collapses to c/b, exactly right for a flat rate.
func solve(a, b, c float64) float64 {
	if c <= 0 {
		return 0
	}
	disc := b*b + 4*a*c
	if disc < 0 {
		disc = 0
	}
	den := b + math.Sqrt(disc)
	if den <= 0 {
		// Only reachable for a segment that offers nothing, which segmentAtCount has
		// already stepped over. Returning zero keeps the arrival at the segment start
		// rather than producing a NaN that would poison every downstream number.
		return 0
	}
	return 2 * c / den
}

// Schedule produces arrival offsets for one runner, in order.
//
// It is not safe for concurrent use: one schedule belongs to one arrival loop.
type Schedule struct {
	profile *Profile
	arrival Arrival
	rng     *rand.Rand

	// consumed is the cumulative count fed into the inverse: an integer count for
	// uniform arrivals, a sum of Exp(1) draws for Poisson.
	consumed float64
	last     time.Duration
	count    int
	done     bool
}

// NewSchedule returns the arrival schedule for a profile. rng is required for Poisson
// arrivals and is ignored for uniform; it is seeded by the caller and the seed is
// recorded in the result, so a run can be reproduced exactly.
func NewSchedule(p *Profile, arrival Arrival, rng *rand.Rand) (*Schedule, error) {
	if p == nil {
		return nil, errs.New(errs.CodeInternal, "a schedule needs a profile")
	}
	switch arrival {
	case ArrivalUniform, ArrivalPoisson:
	default:
		return nil, errs.New(errs.CodeConfigInvalidValue, "unknown arrival process %q", arrival).
			WithPath("/run/arrival").
			WithHint("use uniform or poisson")
	}
	if arrival == ArrivalPoisson && rng == nil {
		return nil, errs.New(errs.CodeInternal, "poisson arrivals need a seeded random source")
	}
	return &Schedule{profile: p, arrival: arrival, rng: rng}, nil
}

// Next returns the offset of the next arrival, reporting false once the profile is
// exhausted. Offsets are non-decreasing: a Poisson process can legitimately place two
// arrivals within the same nanosecond, and that is passed through rather than hidden.
func (s *Schedule) Next() (time.Duration, bool) {
	if s.done {
		return 0, false
	}
	switch s.arrival {
	case ArrivalPoisson:
		s.consumed += s.rng.ExpFloat64()
	case ArrivalUniform:
		s.consumed++
	}
	at, ok := s.profile.Invert(s.consumed)
	if !ok {
		s.done = true
		return 0, false
	}
	if at < s.last {
		at = s.last
	}
	s.last = at
	s.count++
	return at, true
}

// Count is how many arrivals have been produced so far.
func (s *Schedule) Count() int { return s.count }

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func finite(what string, v float64) *errs.Error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return errs.New(errs.CodeConfigInvalidValue, "%s must be a finite number, got %v", what, v)
	}
	return nil
}

func stagePath(i int) string {
	return "/executor/stages/" + strconv.Itoa(i)
}
