package analysis

import (
	"fmt"
	"math"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Finding codes raised by validity evaluation. They are listed in docs/ERRORS.md.
const (
	CodeGeneratorBehind      = "GENERATOR_BEHIND"
	CodeClientCapped         = "CLIENT_CAPPED"
	CodeTargetSaturated      = "TARGET_SATURATED"
	CodeSameHostTarget       = "SAME_HOST_TARGET"
	CodeInsecureTLS          = "INSECURE_TLS"
	CodeRateLimited          = "RATE_LIMITED"
	CodeRunAborted           = "RUN_ABORTED"
	CodeTelemetryUnavailable = "TELEMETRY_UNAVAILABLE"
	CodeLittlesLaw           = "LITTLES_LAW_INCONSISTENT"
)

// evaluateValidity decides whether the run describes the target or describes us.
//
// This is the first thing a reader should see. An invalid run measured the load
// generator: every latency in it includes our own delay, so its numbers say nothing
// about the target and must not be interpreted.
func evaluateValidity(r *result.Result, in Inputs) result.Validity {
	v := result.Validity{State: result.ValidityValid}
	// A generator is "behind" once its dispatch lag is a material fraction of a bucket,
	// floored at 5ms so a very short bucket does not make the rule hair-trigger.
	limit := msOf(5 * time.Millisecond)
	if fivePercent := r.Run.BucketMS * 0.05; fivePercent > limit {
		limit = fivePercent
	}

	for i := range r.Runners {
		rn := &r.Runners[i]
		v.Findings = append(v.Findings, executorFindings(rn, r.Run.BucketMS, limit)...)
		if f, ok := littlesLaw(rn); ok {
			v.Findings = append(v.Findings, f)
		}

		for _, t := range rn.Targets {
			if t.Scope == result.ScopeLoopback {
				v.Findings = append(v.Findings, result.Finding{
					Code:     CodeSameHostTarget,
					Severity: result.SeverityWarn,
					Message:  fmt.Sprintf("%s resolves to loopback, so the generator and the target shared this machine's CPU", t.Host),
					Fix:      "results are indicative rather than a capacity number; run the generator on a separate host to measure capacity",
				})
				break
			}
		}

		if rn.HTTP != nil {
			if rn.HTTP.InsecureTLS {
				v.Findings = append(v.Findings, result.Finding{
					Code: CodeInsecureTLS, Severity: result.SeverityWarn,
					Message: "certificate verification was disabled for this run",
					Fix:     "remove insecure_skip_verify, or supply the CA bundle with tls.ca_file",
				})
			}
			if n := rn.HTTP.StatusHistogram["429"]; n > 0 && rn.Summary.N > 0 {
				if ratio := float64(n) / float64(rn.Summary.N); ratio > 0.05 {
					v.Findings = append(v.Findings, result.Finding{
						Code: CodeRateLimited, Severity: result.SeverityWarn,
						Message: fmt.Sprintf("%.0f%% of responses were 429: this run measured the rate limiter, not the service", ratio*100),
						Fix:     "lower the rate below the limit, or have the limiter exempt the test's user agent",
					})
				}
			}
		}
	}

	v.Findings = append(v.Findings, telemetryFindings(r, in)...)

	if r.Run.Status == result.StatusAborted {
		v.Findings = append(v.Findings, result.Finding{
			Code: CodeRunAborted, Severity: result.SeverityWarn,
			Message: "the abort guard stopped this run early, so it covers less than the configured duration",
		})
	}

	v.Findings = dedupeFindings(v.Findings)

	// Severity decides the state: any error invalidates, any warning degrades.
	for _, f := range v.Findings {
		switch f.Severity {
		case result.SeverityError:
			v.State = result.ValidityInvalid
			return v
		case result.SeverityWarn:
			v.State = result.ValidityDegraded
		}
	}
	return v
}

// executorFindings judges whether one runner's executor kept its schedule.
func executorFindings(rn *result.Runner, bucketMS, lagLimit float64) []result.Finding {
	if rn.Executor == nil {
		return nil
	}
	var out []result.Finding
	if lag := rn.Executor.DispatchLagMS.P99; lag > lagLimit {
		out = append(out, result.Finding{
			Code:     CodeGeneratorBehind,
			Severity: result.SeverityError,
			Message: fmt.Sprintf(
				"the %s runner dispatched arrivals %.1fms late at p99, above the %.1fms limit: the generator missed its own schedule, so every latency it reports includes that delay",
				rn.Name, lag, lagLimit),
			Detail: map[string]any{"runner": rn.Name, "dispatch_lag_p99_ms": lag, "limit_ms": lagLimit},
			Fix:    "lower the rate, raise max_in_flight, or run the generator on a less loaded machine",
		})
	}

	if rn.Executor.DroppedRatio > 0.01 {
		// Dropped arrivals read two ways, and the difference decides whether the run is
		// worthless or is a genuine finding about the target. With service time flat we
		// hit our own ceiling; with service time rising the target was saturating,
		// which is worth knowing.
		if serviceTimeRose(rn) {
			out = append(out, result.Finding{
				Code:     CodeTargetSaturated,
				Severity: result.SeverityWarn,
				Message: fmt.Sprintf(
					"the %s runner dropped %.1f%% of arrivals while service time rose: the target could not keep up",
					rn.Name, rn.Executor.DroppedRatio*100),
				Detail: map[string]any{"runner": rn.Name, "dropped_ratio": rn.Executor.DroppedRatio},
				Fix:    "this is a finding about the target, not a fault in the run; lower the rate to measure below saturation",
			})
		} else {
			needed := NeededInFlight(rn, bucketMS)
			out = append(out, result.Finding{
				Code:     CodeClientCapped,
				Severity: result.SeverityError,
				Message: fmt.Sprintf(
					"the %s runner dropped %.1f%% of arrivals while service time stayed flat: the ceiling was ours, not the target's",
					rn.Name, rn.Executor.DroppedRatio*100),
				Detail: map[string]any{
					"runner": rn.Name, "dropped_ratio": rn.Executor.DroppedRatio,
					"max_in_flight": rn.Executor.MaxInFlight, "needed_in_flight": needed,
				},
				Fix: fmt.Sprintf("raise %s.executor.max_in_flight to about %d (Little's Law: %.0f req/s x %.0fms p99 service time)",
					rn.Name, needed, OfferedRPS(rn, bucketMS), rn.Summary.Service.P99),
			})
		}
	}

	return out
}

// NeededInFlight is the concurrency the offered rate actually needed, by Little's Law:
// in flight = rate x service time, at p99 so that the tail does not starve the pool.
// It is always at least one more than what was configured, since it is only asked
// for when what was configured proved too few.
func NeededInFlight(rn *result.Runner, bucketMS float64) int {
	if rn.Executor == nil {
		return 0
	}
	needed := int(math.Ceil(OfferedRPS(rn, bucketMS) * rn.Summary.Service.P99 / 1000 * 1.2))
	if needed <= rn.Executor.MaxInFlight {
		needed = rn.Executor.MaxInFlight * 2
	}
	return needed
}

// OfferedRPS is the rate the schedule asked for over the measured window, which is
// what a pool has to be sized for - not the lower rate it managed.
func OfferedRPS(rn *result.Runner, bucketMS float64) float64 {
	var offered int64
	var buckets int
	for i := range rn.Buckets {
		b := &rn.Buckets[i]
		if b.Warmup {
			continue
		}
		offered += b.Offered
		buckets++
	}
	if buckets == 0 || offered == 0 || bucketMS <= 0 {
		return rn.Summary.AchievedRPS
	}
	return float64(offered) / (float64(buckets) * bucketMS / 1000)
}

// serviceTimeRose reports whether the target slowed in the buckets where arrivals
// were dropped, compared with the steady buckets where they were not.
//
// A rise of half again means the drops were the target saturating; anything flatter
// means they were our own ceiling. When every measured bucket dropped arrivals, the
// first few are the only baseline there is.
func serviceTimeRose(rn *result.Runner) bool {
	var dropping, calm []float64
	for i := range rn.Buckets {
		b := &rn.Buckets[i]
		if !eligible(b) {
			continue
		}
		if b.Dropped > 0 {
			dropping = append(dropping, b.Service.P99)
		} else {
			calm = append(calm, b.Service.P99)
		}
	}
	if len(dropping) == 0 {
		return false
	}
	if len(calm) < 3 {
		// Everything dropped. Compare the earliest buckets with the rest, which is
		// the cautious reading: a flat line is judged ours, not the target's.
		if len(dropping) < 6 {
			return false
		}
		head := min(5, len(dropping)/3)
		calm, dropping = dropping[:head], dropping[head:]
	}
	base := median(calm)
	if base <= 0 {
		return median(dropping) > 0
	}
	return median(dropping) >= 1.5*base
}

// littlesLaw cross-checks measured concurrency against rate x service time.
//
// Little's Law says mean concurrency equals throughput times mean time in the system.
// The peak concurrency seen in a bucket can never be below the mean, so a bucket
// where rate x mean service time exceeds its peak in-flight count by more than 20%
// is physically inconsistent: the timings or the counts are wrong, most often because
// work waited somewhere neither measured - an unnoticed queue or an undersized pool.
func littlesLaw(rn *result.Runner) (result.Finding, bool) {
	var est, obs []float64
	for i := range rn.Buckets {
		b := &rn.Buckets[i]
		if !eligible(b) || b.InFlightMax < 1 {
			continue
		}
		est = append(est, b.RPS*b.Service.Mean/1000)
		obs = append(obs, float64(b.InFlightMax))
	}
	if len(est) < 5 {
		return result.Finding{}, false
	}
	e, o := median(est), median(obs)
	if o < 1 || e <= 1.2*o {
		return result.Finding{}, false
	}
	return result.Finding{
		Code:     CodeLittlesLaw,
		Severity: result.SeverityWarn,
		Message: fmt.Sprintf(
			"the %s runner's throughput and service time imply about %.1f operations in flight, but at most %.0f were: something waited where it was not measured",
			rn.Name, e, o),
		Detail: map[string]any{"runner": rn.Name, "estimated_in_flight": round2(e), "observed_in_flight": o},
		Fix:    "check the connection pool size and any queue in front of the target",
	}, true
}

// telemetryFindings reports every sampler the configuration asked for that did not
// deliver. The run is still usable - verdicts fall back to latency alone - but its
// confidence is lower, and the reader should know why.
func telemetryFindings(r *result.Result, in Inputs) []result.Finding {
	var out []result.Finding
	for _, name := range in.Telemetry {
		var s *result.SamplerSeries
		if r.Telemetry != nil {
			switch name {
			case SourcePostgres:
				s = r.Telemetry.Postgres
			case SourceMySQL:
				s = r.Telemetry.MySQL
			case SourceRedis:
				s = r.Telemetry.Redis
			}
		}
		reason := "it produced no samples"
		switch {
		case s != nil && s.Available && len(s.Samples) > 0:
			continue
		case s != nil && s.Reason != "":
			reason = s.Reason
		}
		out = append(out, result.Finding{
			Code:     CodeTelemetryUnavailable,
			Severity: result.SeverityWarn,
			Message:  fmt.Sprintf("the %s sampler was unavailable: %s", name, reason),
			Detail:   map[string]any{"sampler": name},
			Fix:      telemetryFix(name),
		})
	}
	return out
}

func telemetryFix(name string) string {
	switch name {
	case SourcePostgres:
		return "GRANT pg_read_all_stats TO <user>, or point telemetry.postgres.dsn at a role that has it"
	case SourceMySQL:
		return "GRANT PROCESS ON *.* TO <user>, or point telemetry.mysql.dsn at a user that has it"
	case SourceRedis:
		return "allow the INFO command for the configured user (ACL +info)"
	default:
		return "check the sampler's connection settings"
	}
}

// dedupeFindings collapses findings that say the same thing.
//
// A caveat about the run as a whole - three runners all pointing at loopback, say - is
// discovered once per runner but is one fact. Repeating it makes a report look like it
// found three problems.
func dedupeFindings(in []result.Finding) []result.Finding {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, f := range in {
		key := f.Code + "\x00" + f.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}
