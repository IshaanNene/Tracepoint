package capacity

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Knobs.
const (
	KnobRate        = "rate"
	KnobConcurrency = "concurrency"
)

// The break rules' constants (spec §5.6). METHODOLOGY.md explains each.
const (
	// AbortErrorRatio ends the whole search: past it the target is failing, not slow.
	AbortErrorRatio = 0.5
	// PlateauRatio: under the open model, a level that completes less than this share
	// of what it offered has plateaued.
	PlateauRatio = 0.9
	// PlateauEfficiency: under the closed model offered load is an output, so a level
	// has plateaued when its extra users bought less than this share of the throughput
	// proportional scaling from the level below would give.
	PlateauEfficiency = 0.1
	// KneeFactor: the knee is the first level whose p99 exceeds this multiple of the
	// lowest p99 at any lower level.
	KneeFactor = 2
)

// Budget is one runner's SLO, in the units a level is measured in.
type Budget struct {
	P95MS     *float64
	P99MS     *float64
	ErrorRate *float64
}

// Measure is one runner's steady window at one level. Latencies are response time,
// which is what SLOs are judged on.
type Measure struct {
	Runner     string
	N          int64
	Achieved   float64
	P95MS      float64
	P99MS      float64
	ErrorRatio float64
}

// Level is everything a level's verdict depends on.
type Level struct {
	Knob     string
	Value    float64
	Runner   string // the runner the knob drives
	Measures []Measure
	Budgets  map[string]Budget
	// Below is the highest lower level that held, for the closed-model plateau.
	Below *Point
}

// Verdict is whether a level held, and if not why. Abort ends the search.
type Verdict struct {
	OK     bool
	Reason string
	Abort  bool
}

// Judge decides whether a level held: every runner within its SLO, and the knob's
// runner keeping up. A level breaks on any SLO breach or a throughput plateau, and the
// search aborts when more than half the knob runner's operations failed.
func Judge(l Level) Verdict {
	var knob *Measure
	for i := range l.Measures {
		if l.Measures[i].Runner == l.Runner {
			knob = &l.Measures[i]
		}
	}
	if knob == nil || knob.N == 0 {
		return Verdict{Reason: fmt.Sprintf("no %s operation completed at this level", l.Runner)}
	}
	if knob.ErrorRatio > AbortErrorRatio {
		return Verdict{Abort: true, Reason: fmt.Sprintf("%s error rate %s is above %s: the target is failing, so the search stopped",
			l.Runner, pct(knob.ErrorRatio), pct(AbortErrorRatio))}
	}

	var reasons []string
	for _, m := range l.Measures {
		b, ok := l.Budgets[m.Runner]
		if !ok {
			continue
		}
		if b.P95MS != nil && m.P95MS > *b.P95MS {
			reasons = append(reasons, fmt.Sprintf("%s p95 %sms > %sms", m.Runner, num(m.P95MS), num(*b.P95MS)))
		}
		if b.P99MS != nil && m.P99MS > *b.P99MS {
			reasons = append(reasons, fmt.Sprintf("%s p99 %sms > %sms", m.Runner, num(m.P99MS), num(*b.P99MS)))
		}
		if b.ErrorRate != nil && m.ErrorRatio > *b.ErrorRate {
			reasons = append(reasons, fmt.Sprintf("%s error rate %s > %s", m.Runner, pct(m.ErrorRatio), pct(*b.ErrorRate)))
		}
	}

	switch l.Knob {
	case KnobRate:
		if knob.Achieved < PlateauRatio*l.Value {
			reasons = append(reasons, fmt.Sprintf("plateau: %s completed %s/s, %.0f%% of the %s/s offered",
				l.Runner, num(knob.Achieved), 100*knob.Achieved/l.Value, num(l.Value)))
		}
	case KnobConcurrency:
		if b := l.Below; b != nil && b.N > 0 && b.X > 0 && l.Value > b.N {
			gain, want := knob.Achieved/b.X-1, l.Value/b.N-1
			if gain < PlateauEfficiency*want {
				reasons = append(reasons, fmt.Sprintf("plateau: %s users completed %s/s, %.0f%% more than %s users did, for %.0f%% more users",
					num(l.Value), num(knob.Achieved), 100*gain, num(b.N), 100*want))
			}
		}
	}
	if len(reasons) > 0 {
		return Verdict{Reason: strings.Join(reasons, "; ")}
	}
	return Verdict{OK: true}
}

// Tried is one level as run, for the knee.
type Tried struct {
	Value float64
	Phase string
	P99MS float64
}

// Knee is the first level - by value, confirmation re-runs aside - whose p99 exceeds
// KneeFactor times the lowest p99 at any lower level: where latency began climbing
// sharply. Nil when latency never did.
func Knee(levels []Tried) *float64 {
	byValue := map[float64]float64{}
	for _, l := range levels {
		if l.Phase == PhaseConfirm || l.P99MS <= 0 {
			continue
		}
		if _, seen := byValue[l.Value]; !seen {
			byValue[l.Value] = l.P99MS
		}
	}
	values := make([]float64, 0, len(byValue))
	for v := range byValue {
		values = append(values, v)
	}
	sort.Float64s(values)
	lowest := math.Inf(1)
	for i, v := range values {
		p := byValue[v]
		if i > 0 && p > KneeFactor*lowest {
			return &v
		}
		lowest = math.Min(lowest, p)
	}
	return nil
}

func pct(r float64) string { return strconv.FormatFloat(100*r, 'f', 1, 64) + "%" }

func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}
