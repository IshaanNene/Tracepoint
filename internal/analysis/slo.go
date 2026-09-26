package analysis

import (
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// evaluateSLO checks the configured budgets against response time - what a user
// feels - over the measured window. A breach makes the command exit 1.
func evaluateSLO(r *result.Result, in Inputs) result.SLO {
	out := result.SLO{Pass: true}
	// A capacity search breaks its SLOs on purpose, to find where they break; each
	// level is judged against them instead, and the whole run is not.
	if in.SLO.IsZero() || r.Capacity != nil {
		return out
	}
	for i := range r.Runners {
		rn := &r.Runners[i]
		target := in.SLO.For(rn.Name)
		if target.IsZero() {
			continue
		}
		if target.P95 != nil {
			addCheck(&out, result.SLOCheck{Runner: rn.Name, Metric: "p95",
				Budget: msOf(target.P95.D()), Actual: rn.Summary.Response.P95})
		}
		if target.P99 != nil {
			addCheck(&out, result.SLOCheck{Runner: rn.Name, Metric: "p99",
				Budget: msOf(target.P99.D()), Actual: rn.Summary.Response.P99})
		}
		if target.ErrorRate != nil {
			addCheck(&out, result.SLOCheck{Runner: rn.Name, Metric: "error_rate",
				Budget: *target.ErrorRate, Actual: rn.Summary.ErrorRatio})
		}
	}
	return out
}

func addCheck(s *result.SLO, c result.SLOCheck) {
	c.Pass = c.Actual <= c.Budget
	if !c.Pass {
		s.Pass = false
	}
	s.Checks = append(s.Checks, c)
}
