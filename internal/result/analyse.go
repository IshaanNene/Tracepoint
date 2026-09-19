package result

import (
	"fmt"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/config"
)

// Analyse derives everything that can be concluded from a recorded run.
//
// It is a pure function of the document, so the same result always yields the same
// findings in the same words - which is what makes verdicts auditable and golden-file
// testable.
//
// This build evaluates run validity and SLO budgets, which is what the exit-code
// contract needs. Hot buckets, incidents, culprit ranking, lagged correlation and real
// verdicts arrive in phase 3 with the telemetry samplers that corroborate them; until
// then the verdict says so rather than guessing.
func Analyse(r *Result, cfg *config.Config) Analysis {
	a := Analysis{
		Validity: evaluateValidity(r, cfg),
		SLO:      evaluateSLO(r, cfg),
	}
	a.Verdict = buildVerdict(r, a)
	return a
}

// evaluateValidity decides whether the run describes the target or describes us.
//
// This is the first thing a reader should see. An invalid run measured the load
// generator: every latency in it includes our own delay, so its numbers say nothing
// about the target and must not be interpreted.
func evaluateValidity(r *Result, cfg *config.Config) Validity {
	v := Validity{State: ValidityValid}
	// A generator is "behind" once its dispatch lag is a material fraction of a bucket,
	// floored at 5ms so a very short bucket does not make the rule hair-trigger.
	limit := ms(5 * time.Millisecond)
	if fivePercent := r.Run.BucketMS * 0.05; fivePercent > limit {
		limit = fivePercent
	}

	for i := range r.Runners {
		rn := &r.Runners[i]
		if rn.Executor == nil {
			continue
		}
		if lag := rn.Executor.DispatchLagMS.P99; lag > limit {
			v.Findings = append(v.Findings, Finding{
				Code:     "GENERATOR_BEHIND",
				Severity: SeverityError,
				Message: fmt.Sprintf(
					"the %s runner dispatched arrivals %.1fms late at p99, above the %.1fms limit: the generator missed its own schedule, so every latency it reports includes that delay",
					rn.Name, lag, limit),
				Detail: map[string]any{"runner": rn.Name, "dispatch_lag_p99_ms": lag, "limit_ms": limit},
				Fix:    "lower the rate, raise max_in_flight, or run the generator on a less loaded machine",
			})
		}

		if rn.Executor.DroppedRatio > 0.01 {
			// Dropped arrivals read two ways, and the difference decides whether the
			// run is worthless or is a genuine finding about the target. With service
			// time flat we hit our own ceiling; with service time rising the target was
			// saturating, which is worth knowing.
			rising := serviceTimeRose(rn)
			switch {
			case rising:
				v.Findings = append(v.Findings, Finding{
					Code:     "TARGET_SATURATED",
					Severity: SeverityWarn,
					Message: fmt.Sprintf(
						"the %s runner dropped %.1f%% of arrivals while service time rose: the target could not keep up",
						rn.Name, rn.Executor.DroppedRatio*100),
					Detail: map[string]any{"runner": rn.Name, "dropped_ratio": rn.Executor.DroppedRatio},
					Fix:    "this is a finding about the target, not a fault in the run; lower the rate to measure below saturation",
				})
			default:
				v.Findings = append(v.Findings, Finding{
					Code:     "CLIENT_CAPPED",
					Severity: SeverityError,
					Message: fmt.Sprintf(
						"the %s runner dropped %.1f%% of arrivals while service time stayed flat: the ceiling was ours, not the target's",
						rn.Name, rn.Executor.DroppedRatio*100),
					Detail: map[string]any{"runner": rn.Name, "dropped_ratio": rn.Executor.DroppedRatio},
					Fix:    littlesLawHint(rn),
				})
			}
		}

		for _, t := range rn.Targets {
			if t.Scope == ScopeLoopback {
				v.Findings = append(v.Findings, Finding{
					Code:     "SAME_HOST_TARGET",
					Severity: SeverityWarn,
					Message:  fmt.Sprintf("%s resolves to loopback, so the generator and the target shared this machine's CPU", t.Host),
					Fix:      "results are indicative rather than a capacity number; run the generator on a separate host to measure capacity",
				})
				break
			}
		}

		if rn.HTTP != nil {
			if rn.HTTP.InsecureTLS {
				v.Findings = append(v.Findings, Finding{
					Code: "INSECURE_TLS", Severity: SeverityWarn,
					Message: "certificate verification was disabled for this run",
					Fix:     "remove insecure_skip_verify, or supply the CA bundle with tls.ca_file",
				})
			}
			if n := rn.HTTP.StatusHistogram["429"]; n > 0 && rn.Summary.N > 0 {
				if ratio := float64(n) / float64(rn.Summary.N); ratio > 0.05 {
					v.Findings = append(v.Findings, Finding{
						Code: "RATE_LIMITED", Severity: SeverityWarn,
						Message: fmt.Sprintf("%.0f%% of responses were 429: this run measured the rate limiter, not the service", ratio*100),
						Fix:     "lower the rate below the limit, or have the limiter exempt the test's user agent",
					})
				}
			}
		}
	}

	if r.Run.Status == StatusAborted {
		v.Findings = append(v.Findings, Finding{
			Code: "RUN_ABORTED", Severity: SeverityWarn,
			Message: "the abort guard stopped this run early, so it covers less than the configured duration",
		})
	}

	// Severity decides the state: any error invalidates, any warning degrades.
	for _, f := range v.Findings {
		switch f.Severity {
		case SeverityError:
			v.State = ValidityInvalid
			return v
		case SeverityWarn:
			v.State = ValidityDegraded
		}
	}
	_ = cfg
	return v
}

// serviceTimeRose reports whether the target slowed over the measured window.
//
// It compares the first and last thirds of the sufficient, post-warm-up buckets. A
// rise means drops were the target saturating; flat means the drops were our own
// ceiling. The distinction is the whole difference between a degraded run and an
// invalid one.
func serviceTimeRose(rn *Runner) bool {
	var vals []float64
	for _, b := range rn.Buckets {
		if b.Warmup || b.Insufficient || b.N == 0 {
			continue
		}
		vals = append(vals, b.Service.P99)
	}
	if len(vals) < 6 {
		return false // too little to say; treat as flat, which is the cautious reading
	}
	third := len(vals) / 3
	early := mean(vals[:third])
	late := mean(vals[len(vals)-third:])
	if early <= 0 {
		return late > 0
	}
	return late > early*1.25
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

// littlesLawHint computes the concurrency the offered rate actually needed. Little's
// Law: in flight = rate x service time.
func littlesLawHint(rn *Runner) string {
	rate := rn.Summary.AchievedRPS
	svc := rn.Summary.Service.P99 / 1000 // seconds
	needed := int(rate*svc) + 1
	if needed <= rn.Executor.MaxInFlight {
		needed = rn.Executor.MaxInFlight * 2
	}
	return fmt.Sprintf("raise %s.executor.max_in_flight to about %d (Little's Law: %.0f req/s x %.0fms service time)",
		rn.Name, needed, rate, rn.Summary.Service.P99)
}

// evaluateSLO checks the configured budgets against response time - what a user
// feels - over the measured window. A breach makes the command exit 1.
func evaluateSLO(r *Result, cfg *config.Config) SLO {
	out := SLO{Pass: true}
	if cfg == nil || cfg.SLO.IsZero() {
		return out
	}
	for i := range r.Runners {
		rn := &r.Runners[i]
		target := cfg.SLO.For(rn.Name)
		if target.IsZero() {
			continue
		}
		if target.P95 != nil {
			out.add(SLOCheck{Runner: rn.Name, Metric: "p95",
				Budget: ms(target.P95.D()), Actual: rn.Summary.Response.P95})
		}
		if target.P99 != nil {
			out.add(SLOCheck{Runner: rn.Name, Metric: "p99",
				Budget: ms(target.P99.D()), Actual: rn.Summary.Response.P99})
		}
		if target.ErrorRate != nil {
			out.add(SLOCheck{Runner: rn.Name, Metric: "error_rate",
				Budget: *target.ErrorRate, Actual: rn.Summary.ErrorRatio})
		}
	}
	return out
}

func (s *SLO) add(c SLOCheck) {
	c.Pass = c.Actual <= c.Budget
	if !c.Pass {
		s.Pass = false
	}
	s.Checks = append(s.Checks, c)
}

// buildVerdict states what can honestly be concluded.
//
// Tier attribution needs hot buckets, incidents and telemetry corroboration, which
// arrive in phase 3. Until then the verdict reports the run's validity and budget
// outcome and says plainly that it cannot attribute a cause - which is a correct
// answer, and a better one than a guess dressed up as a finding.
func buildVerdict(r *Result, a Analysis) Verdict {
	v := Verdict{Caveat: StandingCaveat, Confidence: ConfidenceLow}

	if a.Validity.State == ValidityInvalid {
		v.Bottleneck = BottleneckClient
		v.Confidence = ConfidenceHigh
		v.Summary = "This run is invalid: the load generator, not the target, set the pace. " +
			"Its latency numbers describe TracePoint's own delay and must not be read as a measurement of the target."
		for _, f := range a.Validity.Findings {
			if f.Severity == SeverityError {
				v.Evidence = append(v.Evidence, Evidence{Kind: "validity", Text: f.Message, Data: f.Detail})
				if f.Fix != "" {
					v.NextSteps = append(v.NextSteps, f.Fix)
				}
			}
		}
		v.NextSteps = append(v.NextSteps, "re-run once the generator can keep to its schedule, then read the result")
		return v
	}

	var app *Runner
	for i := range r.Runners {
		if r.Runners[i].Kind == "app" {
			app = &r.Runners[i]
			break
		}
	}

	switch {
	case !a.SLO.Pass:
		v.Bottleneck = BottleneckInconclusive
		v.Confidence = ConfidenceMedium
		v.Summary = "The target missed its latency or error budget. " +
			"Which tier is responsible cannot be attributed from this run: tier attribution needs storage probes and server-side telemetry on the same clock."
		for _, c := range a.SLO.Checks {
			if !c.Pass {
				v.Evidence = append(v.Evidence, Evidence{
					Kind: "slo",
					Text: fmt.Sprintf("%s %s was %.1f against a budget of %.1f", c.Runner, c.Metric, c.Actual, c.Budget),
					Data: c,
				})
			}
		}
		v.NextSteps = append(v.NextSteps,
			"add a db or redis section so the storage tiers are probed alongside the application",
			"enable telemetry so the datastore's own view corroborates the latency")
	case app != nil && app.Summary.N == 0:
		v.Bottleneck = BottleneckInconclusive
		v.Summary = "No operations completed, so there is nothing to conclude."
		v.NextSteps = append(v.NextSteps, "check the target and the configuration with `tracepoint validate`")
	default:
		v.Bottleneck = BottleneckNone
		v.Confidence = ConfidenceMedium
		v.Summary = "Nothing stood out: the run stayed within its budgets and the generator kept to its schedule."
		if app != nil {
			v.Evidence = append(v.Evidence, Evidence{
				Kind: "sample_size",
				Text: fmt.Sprintf("%d operations at %.0f/s, p99 %.1fms, %.2f%% errors",
					app.Summary.N, app.Summary.AchievedRPS, app.Summary.Response.P99, app.Summary.ErrorRatio*100),
			})
		}
	}

	if a.Validity.State == ValidityDegraded {
		v.Confidence = ConfidenceLow
		for _, f := range a.Validity.Findings {
			v.Evidence = append(v.Evidence, Evidence{Kind: "validity", Text: f.Message})
		}
	}
	return v
}
