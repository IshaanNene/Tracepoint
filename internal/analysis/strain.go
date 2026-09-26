package analysis

import (
	"fmt"
	"math"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// findStrain answers "at what load did latency start to degrade?" on a ramping run
// (§5.5.8). It returns nil when the question does not apply: a constant load, no
// application runner, or too little data to form a baseline.
//
// The baseline is the median service-time p99 of the early eligible buckets - the
// first tenth of them, at least StrainEarlyMin and at most StrainEarlyMax - which on
// a ramp are the lightest load the run offered. Strain is the first run of at least
// StrainRun consecutive eligible buckets above StrainFactor times that baseline; its
// first bucket gives the load level, read as that bucket's peak in-flight count and
// throughput. Service time is used because strain is a property of the target: a
// generator falling behind shows up in validity, not here.
func findStrain(r *result.Result, p Params) *result.Strain {
	var app *result.Runner
	for i := range r.Runners {
		if r.Runners[i].Kind == "app" {
			app = &r.Runners[i]
			break
		}
	}
	if app == nil || !ramping(app.Executor) {
		return nil
	}

	var usable []*result.Bucket
	for i := range app.Buckets {
		if b := &app.Buckets[i]; eligible(b) {
			usable = append(usable, b)
		}
	}
	early := int(math.Ceil(float64(len(usable)) * p.StrainEarlyShare))
	early = min(max(early, p.StrainEarlyMin), p.StrainEarlyMax)
	if len(usable) < early+p.StrainRun {
		return nil
	}
	base := make([]float64, 0, early)
	for _, b := range usable[:early] {
		base = append(base, b.Service.P99)
	}
	baseline := median(base)
	limit := p.StrainFactor * baseline

	knob := "rate"
	if app.Executor != nil && app.Executor.Type == "vus" {
		knob = "concurrency"
	}
	level := func(b *result.Bucket) float64 {
		if knob == "concurrency" {
			return float64(b.InFlightMax)
		}
		return b.RPS
	}

	run, first := 0, -1
	for i := early; i < len(usable); i++ {
		b := usable[i]
		consecutive := i > early && usable[i-1].Index == b.Index-1
		switch {
		case b.Service.P99 <= limit:
			run, first = 0, -1
			continue
		case run == 0 || !consecutive:
			run, first = 1, i
		default:
			run++
		}
		if run >= p.StrainRun {
			at := usable[first]
			users, rps := float64(at.InFlightMax), math.Round(at.RPS)
			return &result.Strain{
				Found: true, AtOffsetS: round3(at.OffsetMS / 1000), Users: users, RPS: rps,
				BaselineP99MS: round3(baseline),
				Message: fmt.Sprintf("strain begins at ~%.0f users (~%.0f req/s): p99 service time held above %s, twice the early baseline, from %.0fs",
					users, rps, fmtMS(limit), at.OffsetMS/1000),
				NextWindow: map[string]any{"knob": knob, "from": math.Floor(level(at) / 2), "to": math.Ceil(level(at) * 1.5)},
			}
		}
	}

	var users, rps, top float64
	for _, b := range usable {
		users, rps, top = math.Max(users, float64(b.InFlightMax)), math.Max(rps, math.Round(b.RPS)), math.Max(top, level(b))
	}
	return &result.Strain{
		Found: false, Users: users, RPS: rps, BaselineP99MS: round3(baseline),
		Message:    fmt.Sprintf("no strain up to ~%.0f users (~%.0f req/s): p99 service time never held above %s, twice the early baseline", users, rps, fmtMS(limit)),
		NextWindow: map[string]any{"knob": knob, "from": math.Floor(top), "to": math.Ceil(top * 2)},
	}
}

// ramping reports whether a runner's profile changes level at all. A result written
// before start_target existed is judged by its stages alone.
func ramping(ex *result.ExecutorStats) bool {
	if ex == nil || len(ex.Stages) == 0 {
		return false
	}
	prev := ex.Stages[0].Target
	if ex.StartTarget != nil {
		prev = *ex.StartTarget
	}
	for _, s := range ex.Stages {
		if s.Target != prev {
			return true
		}
		prev = s.Target
	}
	return false
}

func fmtMS(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0fms", v)
	}
	return fmt.Sprintf("%.1fms", v)
}
