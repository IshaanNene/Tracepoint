package result

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Recommend derives machine-applicable next steps from a result (§6.3).
//
// Every recommendation is a fixed rule over fields of the document, with a stable id,
// the observation that triggered it and - where the change is to the test rather than
// to the target - the exact --set overrides that apply it. They are ordered by
// priority: fix the measurement before interpreting it, then gather missing evidence,
// then follow the verdict. Apply one at a time and compare, so a change can be
// attributed.
func Recommend(r *Result) []Recommendation {
	var out []Recommendation
	add := func(rec Recommendation) {
		for _, existing := range out {
			if existing.ID == rec.ID {
				return
			}
		}
		out = append(out, rec)
	}
	a := &r.Analysis

	// 1. The measurement itself.
	for _, f := range a.Validity.Findings {
		runner := detailString(f.Detail, "runner")
		rn := r.RunnerByName(runner)
		switch f.Code {
		case "CLIENT_CAPPED":
			if rn != nil {
				add(raiseInFlight(r, rn, fmt.Sprintf("%s dropped %.1f%% of arrivals while service time stayed flat",
					rn.Name, rn.Executor.DroppedRatio*100)))
			}
		case "GENERATOR_BEHIND":
			if rn == nil || rn.Executor == nil {
				continue
			}
			if rn.Executor.MaxInFlight > 0 && rn.Executor.PeakInFlight >= int64(rn.Executor.MaxInFlight) {
				add(raiseInFlight(r, rn, fmt.Sprintf("%s fell behind its schedule with every worker busy", rn.Name)))
			} else {
				add(scaleRate(r, rn, 0.5, "lower-rate-"+rn.Name,
					fmt.Sprintf("%s fell behind its own schedule with workers to spare, so the generator host itself is the limit", rn.Name)))
			}
		case "TARGET_SATURATED":
			if rn != nil {
				add(scaleRate(r, rn, 0.7, "measure-below-saturation-"+rn.Name,
					fmt.Sprintf("%s saturated: it dropped arrivals while service time rose, so latency above this rate is queueing", rn.Name)))
			}
		case "TELEMETRY_UNAVAILABLE":
			sampler := detailString(f.Detail, "sampler")
			add(Recommendation{ID: "grant-telemetry-" + sampler, Action: ActionConfigure, Why: f.Message + "; fix: " + f.Fix})
		}
	}
	for _, inc := range a.Incidents {
		if inc.Class != IncidentClientLimited {
			continue
		}
		for _, ir := range inc.Runners {
			if !ir.Hot {
				continue
			}
			if rn := r.RunnerByName(ir.Name); rn != nil && rn.Executor != nil {
				add(raiseInFlight(r, rn, fmt.Sprintf("client wait dominated %s in %s", rn.Name, inc.ID)))
			}
		}
	}

	// 2. Missing evidence.
	storage := 0
	for i := range r.Runners {
		if r.Runners[i].Kind == "storage" {
			storage++
		}
	}
	unobserved := 0
	for _, inc := range a.Incidents {
		if inc.Class == IncidentUnobserved {
			unobserved++
		}
	}
	if unobserved > 0 {
		if storage == 0 {
			add(Recommendation{
				ID: "add-storage-probes", Action: ActionConfigure,
				Why: fmt.Sprintf("the application slowed in %d incident(s) but no storage tier was probed, so the cause cannot be attributed; add a db or redis section", unobserved),
			})
		}
		for i := range r.Runners {
			rn := &r.Runners[i]
			if rn.Kind != "storage" || !starvedInIncidents(rn, a.Incidents) {
				continue
			}
			add(probeRate(r, rn, unobserved))
		}
	}
	switch b := a.Verdict.Bottleneck; b {
	case BottleneckDB, BottleneckRedis:
		if sampler := samplerFor(r, b); sampler != "" && !samplerRan(r, sampler) {
			add(Recommendation{
				ID: "enable-telemetry-" + sampler, Action: ActionRerun,
				Why:       fmt.Sprintf("the verdict points at %s on latency alone; %s telemetry would corroborate or refute it", b, sampler),
				Overrides: []string{"telemetry." + sampler + ".enabled=true"},
			})
		}
	}

	// 3. Follow the verdict.
	switch a.Verdict.Bottleneck {
	case BottleneckDB:
		add(Recommendation{ID: "investigate-db", Action: ActionInvestigate, Why: windowsWhy("the database tier", a.Incidents, "db")})
	case BottleneckRedis:
		add(Recommendation{ID: "investigate-redis", Action: ActionInvestigate, Why: windowsWhy("Redis", a.Incidents, "redis")})
	case BottleneckApp:
		add(Recommendation{ID: "profile-app", Action: ActionInvestigate, Why: windowsWhy("the application tier", a.Incidents, "")})
	case BottleneckNone:
		if len(a.SLO.Checks) == 0 {
			add(Recommendation{ID: "configure-slo", Action: ActionConfigure,
				Why: "no budget was configured, so a pass means nothing was checked; add slo.http.p99 and error_rate"})
		}
		if app := firstApp(r); app != nil && a.Validity.State != ValidityInvalid {
			add(scaleRate(r, app, 2, "raise-load",
				"nothing stood out at this load; double it to find where latency starts to degrade"))
		}
	}

	// 4. Caveats that need a human.
	for _, f := range a.Validity.Findings {
		switch f.Code {
		case "SAME_HOST_TARGET":
			add(Recommendation{ID: "separate-generator-host", Action: ActionConfigure,
				Why: "generator and target shared one machine; run the generator elsewhere before quoting a capacity number"})
		case "GENERATOR_STALL":
			add(Recommendation{ID: "dedicated-generator-host", Action: ActionConfigure,
				Why: "the generator was paused by its host; run it where it has dedicated CPU"})
		case "RATE_LIMITED":
			add(Recommendation{ID: "exempt-rate-limiter", Action: ActionConfigure, Why: f.Message})
		}
	}

	for i := range out {
		out[i].Priority = i + 1
	}
	return out
}

// raiseInFlight applies Little's Law to the runner's offered rate and p99 service time.
func raiseInFlight(r *Result, rn *Runner, why string) Recommendation {
	needed := rn.NeededInFlight(r.Run.BucketMS)
	return Recommendation{
		ID:     "raise-max-in-flight-" + rn.Name,
		Action: ActionRerun,
		Why: fmt.Sprintf("%s; Little's Law needs about %d in flight (%.0f req/s x %.0fms p99 service time, +20%%), against %d configured",
			why, needed, rn.OfferedRPS(r.Run.BucketMS), rn.Summary.Service.P99, rn.Executor.MaxInFlight),
		Overrides: []string{fmt.Sprintf("%s.executor.max_in_flight=%d", rn.Name, needed)},
	}
}

// scaleRate recommends a different constant rate when the configuration uses the
// `rate:` shorthand, which an override can change cleanly. A staged profile has no
// single number to override, so the advice becomes a configuration change instead.
func scaleRate(r *Result, rn *Runner, factor float64, id, why string) Recommendation {
	if rate, ok := configuredRate(r, rn.Name); ok {
		next := math.Max(1, math.Round(rate*factor))
		return Recommendation{
			ID: id, Action: ActionRerun,
			Why:       fmt.Sprintf("%s; rate %s -> %s", why, compact(rate), compact(next)),
			Overrides: []string{fmt.Sprintf("%s.executor.rate=%s", rn.Name, compact(next))},
		}
	}
	return Recommendation{ID: id, Action: ActionConfigure,
		Why: fmt.Sprintf("%s; scale %s.executor.stages targets by %.1fx", why, rn.Name, factor)}
}

// probeRate raises a probe until every bucket holds min_samples operations, with 50%
// to spare.
func probeRate(r *Result, rn *Runner, incidents int) Recommendation {
	minSamples := r.Run.MinSamples
	if minSamples <= 0 {
		minSamples = 20
	}
	bucketS := r.Run.BucketMS / 1000
	if bucketS <= 0 {
		bucketS = 1
	}
	target := math.Ceil(float64(minSamples) / bucketS * 1.5)
	why := fmt.Sprintf("the %s probe had too few samples per bucket during %d unobserved incident(s); %d per bucket are needed",
		rn.Name, incidents, minSamples)
	if rate, ok := configuredRate(r, rn.Name); ok && rate < target {
		return Recommendation{
			ID: "raise-probe-rate-" + rn.Name, Action: ActionRerun, Why: why,
			Overrides: []string{fmt.Sprintf("%s.executor.rate=%s", rn.Name, compact(target))},
		}
	}
	return Recommendation{ID: "raise-probe-rate-" + rn.Name, Action: ActionConfigure,
		Why: why + fmt.Sprintf("; raise its rate to at least %s/s", compact(target))}
}

func starvedInIncidents(rn *Runner, incidents []Incident) bool {
	for _, inc := range incidents {
		if inc.Class != IncidentUnobserved {
			continue
		}
		for _, ir := range inc.Runners {
			if ir.Name == rn.Name && ir.Insufficient {
				return true
			}
		}
	}
	return false
}

// configuredRate reads <runner>.executor.rate from the embedded effective
// configuration.
func configuredRate(r *Result, runner string) (float64, bool) {
	if r.Config == nil {
		return 0, false
	}
	root, ok := r.Config.Effective.(map[string]any)
	if !ok {
		return 0, false
	}
	sec, ok := root[runner].(map[string]any)
	if !ok {
		return 0, false
	}
	ex, ok := sec["executor"].(map[string]any)
	if !ok {
		return 0, false
	}
	rate, ok := numberOf(ex["rate"])
	return rate, ok && rate > 0
}

// samplerFor names the telemetry sampler that watches a storage runner's tier.
func samplerFor(r *Result, runner string) string {
	switch runner {
	case "redis":
		return "redis"
	case "db":
		if rn := r.RunnerByName("db"); rn != nil {
			switch rn.Driver {
			case "postgres", "mysql":
				return rn.Driver
			}
		}
	}
	return ""
}

func samplerRan(r *Result, name string) bool {
	if r.Telemetry == nil {
		return false
	}
	switch name {
	case "postgres":
		return r.Telemetry.Postgres != nil
	case "mysql":
		return r.Telemetry.MySQL != nil
	case "redis":
		return r.Telemetry.Redis != nil
	}
	return false
}

func windowsWhy(tier string, incidents []Incident, culprit string) string {
	var windows []string
	for _, inc := range incidents {
		match := culprit == "" && inc.Class == IncidentAppOnly ||
			culprit != "" && ((inc.Culprit != nil && inc.Culprit.Runner == culprit) || inc.Class == IncidentStorageOnly && hotIn(inc, culprit))
		if match {
			windows = append(windows, fmt.Sprintf("%ss-%ss", compact(inc.StartS), compact(inc.EndS)))
		}
	}
	sort.Strings(windows)
	if len(windows) > 5 {
		windows = append(windows[:5], "...")
	}
	if len(windows) == 0 {
		return "the verdict points at " + tier
	}
	return fmt.Sprintf("the verdict points at %s; look at it during %s", tier, joinComma(windows))
}

func hotIn(inc Incident, runner string) bool {
	for _, ir := range inc.Runners {
		if ir.Name == runner && ir.Hot {
			return true
		}
	}
	return false
}

func firstApp(r *Result) *Runner {
	for i := range r.Runners {
		if r.Runners[i].Kind == "app" {
			return &r.Runners[i]
		}
	}
	return nil
}

func detailString(detail any, key string) string {
	m, ok := detail.(map[string]any)
	if !ok {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case int:
		return strconv.Itoa(v)
	}
	return ""
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
