package analysis

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// candidate is one possible answer and the incidents that support it.
type candidate struct {
	key       string // a runner name, "app", "client", "unobserved" or "tie"
	incidents []result.Incident
	buckets   int
	tied      []string
}

// Candidate priority when two answers cover the same number of buckets. A storage tier
// named by correlated incidents outranks the others because it is the most specific
// claim with the most evidence behind it; unobserved comes last because it is the tool
// declining to answer.
func priority(key string) int {
	switch key {
	case "app":
		return 1
	case "client":
		return 2
	case "tie":
		return 3
	case "unobserved":
		return 4
	default:
		return 0
	}
}

// buildVerdict states what can honestly be concluded, from fixed sentence templates
// filled with measured numbers (ADR-005 §5).
func buildVerdict(r *result.Result, a result.Analysis, views []*view, p Params) result.Verdict {
	v := result.Verdict{Caveat: result.StandingCaveat, Confidence: result.ConfidenceLow}

	if a.Validity.State == result.ValidityInvalid {
		return invalidVerdict(a)
	}

	var app *view
	for _, vw := range views {
		if vw.app {
			app = vw
			break
		}
	}
	if app != nil && app.runner.Summary.N == 0 {
		v.Bottleneck = result.BottleneckInconclusive
		v.Summary = "No operations completed, so there is nothing to conclude."
		v.NextSteps = []string{"check the target and the configuration with `tracepoint validate` and `tracepoint doctor`"}
		return v
	}

	impacting, masked := tally(a.Incidents)
	switch {
	case impacting != nil:
		v = verdictFor(*impacting, a, views, r, p)
	case masked != nil:
		v = maskedVerdict(*masked, a, views, r, p)
	case !a.SLO.Pass:
		v = uniformBreachVerdict(a, views, p)
	default:
		v = quietVerdict(a, app)
	}
	v.Caveat = result.StandingCaveat

	for _, c := range a.SLO.Checks {
		if !c.Pass {
			v.Evidence = append(v.Evidence, result.Evidence{
				Kind: "slo",
				Text: fmt.Sprintf("%s %s was %s against a budget of %s", c.Runner, c.Metric, sloValue(c.Metric, c.Actual), sloValue(c.Metric, c.Budget)),
				Data: map[string]any{"runner": c.Runner, "metric": c.Metric, "budget": c.Budget, "actual": c.Actual},
			})
		}
	}

	// A degraded run is still usable, but its caveat caps how much weight the verdict
	// can carry and travels with it as evidence.
	if a.Validity.State == result.ValidityDegraded {
		if v.Confidence == result.ConfidenceHigh {
			v.Confidence = result.ConfidenceMedium
		}
		for _, f := range a.Validity.Findings {
			if f.Severity != result.SeverityInfo {
				v.Evidence = append(v.Evidence, result.Evidence{Kind: "validity", Text: f.Message})
			}
		}
	}
	return v
}

func invalidVerdict(a result.Analysis) result.Verdict {
	v := result.Verdict{
		Bottleneck: result.BottleneckClient,
		Confidence: result.ConfidenceHigh,
		Caveat:     result.StandingCaveat,
		Summary: "This run is invalid: the load generator, not the target, set the pace. " +
			"Its latency numbers describe TracePoint's own delay and must not be read as a measurement of the target.",
	}
	for _, f := range a.Validity.Findings {
		if f.Severity == result.SeverityError {
			v.Evidence = append(v.Evidence, result.Evidence{Kind: "validity", Text: f.Message, Data: detailMap(f.Detail)})
			if f.Fix != "" {
				v.NextSteps = append(v.NextSteps, f.Fix)
			}
		}
	}
	v.NextSteps = append(v.NextSteps, "re-run once the generator can keep to its schedule, then read the result")
	return v
}

// tally groups the incidents by the answer each supports and picks the best-supported
// one, separately for incidents that reached users and for those that did not.
func tally(incidents []result.Incident) (impacting, masked *candidate) {
	app := map[string]*candidate{}
	storage := map[string]*candidate{}
	for _, inc := range incidents {
		width := inc.EndIndex - inc.StartIndex + 1
		var key string
		var tied []string
		target := app
		switch inc.Class {
		case result.IncidentCorrelated:
			switch {
			case inc.Culprit == nil:
				key = "tie"
			case len(inc.Culprit.TiedWith) > 0:
				key = "tie"
				tied = append([]string{inc.Culprit.Runner}, inc.Culprit.TiedWith...)
			default:
				key = inc.Culprit.Runner
			}
		case result.IncidentAppOnly:
			key = "app"
		case result.IncidentClientLimited:
			key = "client"
		case result.IncidentUnobserved:
			key = "unobserved"
		case result.IncidentStorageOnly:
			target = storage
			for _, rn := range inc.Runners {
				if rn.Hot {
					key = rn.Name
					break
				}
			}
		}
		c := target[key]
		if c == nil {
			c = &candidate{key: key}
			target[key] = c
		}
		c.incidents = append(c.incidents, inc)
		c.buckets += width
		for _, t := range tied {
			if !contains(c.tied, t) {
				c.tied = append(c.tied, t)
			}
		}
	}
	return best(app), best(storage)
}

func best(m map[string]*candidate) *candidate {
	if len(m) == 0 {
		return nil
	}
	all := make([]*candidate, 0, len(m))
	for _, c := range m {
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].buckets != all[j].buckets {
			return all[i].buckets > all[j].buckets
		}
		if priority(all[i].key) != priority(all[j].key) {
			return priority(all[i].key) < priority(all[j].key)
		}
		return all[i].key < all[j].key
	})
	// Two storage tiers each named by an equal share of correlated incidents is a tie
	// at the level of the whole run.
	if len(all) > 1 && all[0].buckets == all[1].buckets &&
		priority(all[0].key) == 0 && priority(all[1].key) == 0 {
		return &candidate{
			key: "tie", incidents: append(append([]result.Incident(nil), all[0].incidents...), all[1].incidents...),
			buckets: all[0].buckets + all[1].buckets, tied: []string{all[0].key, all[1].key},
		}
	}
	return all[0]
}

func verdictFor(c candidate, a result.Analysis, views []*view, r *result.Result, p Params) result.Verdict {
	v := result.Verdict{}
	n := len(c.incidents)
	secs := incidentSeconds(c.incidents)
	total := countImpacting(a.Incidents)

	switch c.key {
	case "app":
		v.Bottleneck = result.BottleneckApp
		v.Summary = fmt.Sprintf(
			"The application tier itself is the most likely source of the slowdown. In %s covering %s, application latency rose while every storage probe stayed healthy on sufficient data, so neither the database nor the cache moved with it.",
			plural(n, "incident"), seconds(secs))
		v.NextSteps = []string{
			"profile the application during the incident windows: CPU, garbage collection, locks and thread or worker pools",
			"check the application's own downstream calls that TracePoint does not probe, such as other services or external APIs",
			"re-run with the same seed after a fix and compare the two results",
		}
	case "client":
		v.Bottleneck = result.BottleneckClient
		v.Summary = fmt.Sprintf(
			"The load generator, not the target, limited this run. In %s covering %s, time spent waiting inside TracePoint before a request was sent dominated what a user would have felt, so those slow periods say nothing about the target.",
			plural(n, "incident"), seconds(secs))
		v.NextSteps = []string{
			"raise max_in_flight to what Little's Law needs (rate x p99 service time), or lower the rate",
			"check the connection pool sizes against the worker count",
		}
	case "unobserved":
		v.Bottleneck = result.BottleneckInconclusive
		v.Summary = fmt.Sprintf(
			"The application slowed in %s covering %s, but the storage probes had too little data at those moments to say which tier was responsible. That is a correct answer, not a failure: the evidence to attribute it was not collected.",
			plural(n, "incident"), seconds(secs))
		v.NextSteps = []string{
			"add a db or redis section so each storage tier is probed alongside the application",
			"raise the probes' rates so every bucket holds at least run.min_samples operations",
		}
	case "tie":
		v.Bottleneck = result.BottleneckInconclusive
		names := c.tied
		if len(names) == 0 {
			names = []string{"more than one storage tier"}
		}
		v.Summary = fmt.Sprintf(
			"The application slowed together with %s in %s covering %s, and the evidence does not single one out: they went hot at the same time and by comparable margins.",
			joinAnd(names), plural(n, "incident"), seconds(secs))
		v.NextSteps = []string{
			"enable telemetry for each storage tier so server-side signals can break the tie",
			"re-run with one storage tier's probe rate raised, to see which one moves first",
		}
	default:
		v.Bottleneck = c.key
		tier := tierName(c.key)
		lead := leadPhrase(c.incidents, c.key)
		v.Summary = fmt.Sprintf(
			"The %s is the most likely source of the slowdown. %s, covering %s, application latency rose together with the %s probe, which %s.%s",
			tier, shareOf(n, total), seconds(secs), c.key, lead, corroborationPhrase(c.incidents, c.key))
		v.NextSteps = storageNextSteps(c.key, c.incidents)
	}

	for _, inc := range topIncidents(c.incidents, 3) {
		v.Evidence = append(v.Evidence, incidentEvidence(inc))
	}
	if c.key == "db" || c.key == "redis" {
		for _, corr := range a.Correlation {
			if corr.Storage == c.key && corr.NBuckets >= p.MinPairs {
				v.Evidence = append(v.Evidence, correlationEvidence(corr))
			}
		}
		for _, sig := range signalsFor(c.incidents, c.key) {
			v.Evidence = append(v.Evidence, result.Evidence{
				Kind: "telemetry",
				Text: fmt.Sprintf("%s %s moved from %s to %s: %s", sig.Source, sig.Signal, num(sig.Baseline), num(sig.Value), sig.Note),
				Data: map[string]any{"source": sig.Source, "signal": sig.Signal, "value": sig.Value, "baseline": sig.Baseline},
			})
		}
	}
	v.Evidence = append(v.Evidence, sampleEvidence(views))
	v.Confidence = confidence(c, a, views, r, p)
	return v
}

func maskedVerdict(c candidate, a result.Analysis, views []*view, r *result.Result, p Params) result.Verdict {
	v := result.Verdict{Bottleneck: c.key}
	v.Summary = fmt.Sprintf(
		"No slowdown reached users, but the %s was slow on its own in %s covering %s while the application stayed within its threshold: a bottleneck that has not yet reached users and is likely to as load grows.%s",
		tierName(c.key), plural(len(c.incidents), "incident"), seconds(incidentSeconds(c.incidents)),
		corroborationPhrase(c.incidents, c.key))
	for _, inc := range topIncidents(c.incidents, 3) {
		v.Evidence = append(v.Evidence, incidentEvidence(inc))
	}
	for _, sig := range signalsFor(c.incidents, c.key) {
		v.Evidence = append(v.Evidence, result.Evidence{
			Kind: "telemetry",
			Text: fmt.Sprintf("%s %s moved from %s to %s: %s", sig.Source, sig.Signal, num(sig.Baseline), num(sig.Value), sig.Note),
		})
	}
	v.Evidence = append(v.Evidence, sampleEvidence(views))
	v.NextSteps = append(storageNextSteps(c.key, c.incidents),
		"raise the application's load to see whether this reaches users, with a capacity search if one is configured")
	v.Confidence = confidence(c, a, views, r, p)
	// Nothing reached users, so this is a warning about the future, not an attribution
	// of something that happened; it never carries the highest weight.
	if v.Confidence == result.ConfidenceHigh {
		v.Confidence = result.ConfidenceMedium
	}
	return v
}

// uniformBreachVerdict covers a budget that was missed with no episode standing out
// from the run's own baseline: the whole run was slow, evenly.
func uniformBreachVerdict(a result.Analysis, views []*view, p Params) result.Verdict {
	v := result.Verdict{Bottleneck: result.BottleneckInconclusive, Confidence: result.ConfidenceLow}
	v.Summary = "The target missed a budget, but no episode stood out from the run's own baseline: it was slow evenly throughout, so there is no moment at which one tier moved and another did not."
	// A strong whole-run correlation is mentioned but never promoted to an answer on
	// its own: both series respond to the same load, so rho at lag 0 across a run is
	// weaker evidence than it looks (ADR-005 §4).
	for _, c := range a.Correlation {
		if c.Lean == LeanStorage && math.Abs(c.Rho) >= p.StrongRho {
			v.Summary += fmt.Sprintf(" Over the whole run application latency tracked the %s probe (Spearman rho %.2f at lag %+d), which leans towards the %s but is not enough to name it.",
				c.Storage, c.Rho, c.BestLag, tierName(c.Storage))
			v.Evidence = append(v.Evidence, correlationEvidence(c))
			break
		}
	}
	v.Evidence = append(v.Evidence, sampleEvidence(views))
	v.NextSteps = []string{
		"run a ramp from a low rate so the point where latency starts to climb becomes visible",
		"enable telemetry for each storage tier so the datastores' own view can be compared",
	}
	return v
}

func quietVerdict(a result.Analysis, app *view) result.Verdict {
	v := result.Verdict{Bottleneck: result.BottleneckNone, Confidence: result.ConfidenceMedium}
	v.Summary = "Nothing stood out: the run stayed within its budgets, the generator kept to its schedule, and no tier's latency departed from its own baseline."
	if app != nil {
		s := app.runner.Summary
		v.Evidence = append(v.Evidence, result.Evidence{
			Kind: "sample_size",
			Text: fmt.Sprintf("%d operations at %.0f/s, p99 %.1fms, %.2f%% errors",
				s.N, s.AchievedRPS, s.Response.P99, s.ErrorRatio*100),
			Data: map[string]any{"runner": app.runner.Name, "n": s.N, "p99_ms": s.Response.P99},
		})
		if sufficiency([]*view{app}) >= 0.9 && eligibleCount(app) >= 30 {
			v.Confidence = result.ConfidenceHigh
		}
	}
	if a.SLO.Checks == nil {
		v.NextSteps = append(v.NextSteps, "configure slo budgets so the run has something to pass or fail against")
	}
	v.NextSteps = append(v.NextSteps, "raise the rate, or run a ramp, to find where latency starts to degrade")
	return v
}

// confidence applies the rubric of ADR-005 §5: sufficient samples, consistency (at
// least two incidents agreeing, or a strong whole-run correlation) and corroboration
// (server-side telemetry, or healthy probes for an application verdict). Three is
// high, two is medium, fewer is low. Answers that decline to attribute are always low.
func confidence(c candidate, a result.Analysis, views []*view, r *result.Result, p Params) string {
	if c.key == "unobserved" || c.key == "tie" {
		return result.ConfidenceLow
	}
	involved := []*view{}
	for _, v := range views {
		if v.app || v.runner.Name == c.key {
			involved = append(involved, v)
		}
	}
	points := 0
	if sufficiency(involved) >= 0.9 {
		points++
	}
	consistent := len(c.incidents) >= 2
	for _, corr := range a.Correlation {
		if corr.Storage == c.key && corr.Lean == LeanStorage && math.Abs(corr.Rho) >= p.StrongRho {
			consistent = true
		}
	}
	if consistent {
		points++
	}
	if corroborated(c, a, r) {
		points++
	}
	switch {
	case points >= 3:
		return result.ConfidenceHigh
	case points == 2:
		return result.ConfidenceMedium
	default:
		return result.ConfidenceLow
	}
}

func corroborated(c candidate, a result.Analysis, r *result.Result) bool {
	switch c.key {
	case "app":
		// The storage tiers are exonerated by their probes; telemetry that stayed quiet
		// through every incident exonerates them a second, independent way.
		if !anyDatastoreTelemetry(r) {
			return false
		}
		for _, inc := range c.incidents {
			for _, s := range inc.Telemetry {
				if runnerForSource(s.Source) != "" {
					return false
				}
			}
		}
		return true
	case "client":
		for _, f := range a.Validity.Findings {
			if f.Code == CodeClientCapped || f.Code == CodeGeneratorBehind || f.Code == CodeTargetSaturated {
				return true
			}
		}
		for i := range r.Runners {
			if e := r.Runners[i].Executor; e != nil && e.Dropped > 0 {
				return true
			}
		}
		return false
	default:
		for _, inc := range c.incidents {
			if corroborates(inc.Telemetry, c.key) {
				return true
			}
		}
		return false
	}
}

func anyDatastoreTelemetry(r *result.Result) bool {
	t := r.Telemetry
	if t == nil {
		return false
	}
	for _, s := range []*result.SamplerSeries{t.Postgres, t.MySQL, t.Redis} {
		if s != nil && s.Available && len(s.Samples) > 0 {
			return true
		}
	}
	return false
}

// sufficiency is the share of the runners' measured buckets that held enough samples.
func sufficiency(views []*view) float64 {
	var total, ok int
	for _, v := range views {
		for i := range v.runner.Buckets {
			b := &v.runner.Buckets[i]
			if b.Warmup {
				continue
			}
			total++
			if eligible(b) {
				ok++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(ok) / float64(total)
}

func eligibleCount(v *view) int {
	n := 0
	for i := range v.runner.Buckets {
		if eligible(&v.runner.Buckets[i]) {
			n++
		}
	}
	return n
}

func storageNextSteps(runner string, incidents []result.Incident) []string {
	windows := make([]string, 0, 3)
	for _, inc := range topIncidents(incidents, 3) {
		windows = append(windows, fmt.Sprintf("%s-%s", seconds(inc.StartS), seconds(inc.EndS)))
	}
	at := strings.Join(windows, ", ")
	corroborated := len(signalsFor(incidents, runner)) > 0
	switch runner {
	case "db":
		out := []string{
			fmt.Sprintf("look at the database during %s: lock waits, long transactions, slow-query log and pg_stat_activity or the processlist", at),
		}
		if !corroborated {
			out = append(out, "enable telemetry for the database, so lock waits and pool saturation are sampled on the same clock")
		}
		return append(out, "check the application's database pool size against its concurrency")
	case "redis":
		out := []string{
			fmt.Sprintf("look at Redis during %s: SLOWLOG, blocking commands, persistence (fork) events and memory pressure", at),
		}
		if !corroborated {
			out = append(out, "enable telemetry for Redis, so blocked clients and throughput are sampled on the same clock")
		}
		return out
	default:
		return []string{fmt.Sprintf("look at the %s tier during %s", runner, at)}
	}
}

func incidentEvidence(inc result.Incident) result.Evidence {
	var hot []string
	for _, rn := range inc.Runners {
		if rn.Hot {
			hot = append(hot, fmt.Sprintf("%s p99 %s", rn.Name, ms(rn.PeakP99MS)))
		}
	}
	text := fmt.Sprintf("%s (%s) from %s to %s: %s", inc.ID, inc.Class, seconds(inc.StartS), seconds(inc.EndS), strings.Join(hot, ", "))
	if inc.Culprit != nil && inc.Culprit.LeadBuckets > 0 {
		text += fmt.Sprintf("; %s led by %s", inc.Culprit.Runner, plural(inc.Culprit.LeadBuckets, "bucket"))
	}
	return result.Evidence{
		Kind: "incident",
		Text: text,
		Data: map[string]any{"id": inc.ID, "class": inc.Class, "start_i": inc.StartIndex, "end_i": inc.EndIndex},
	}
}

func correlationEvidence(c result.Correlation) result.Evidence {
	return result.Evidence{
		Kind: "correlation",
		Text: fmt.Sprintf("over the whole run, application p99 and %s p99 had Spearman rho %.2f at lag %+d across %d buckets",
			c.Storage, c.Rho, c.BestLag, c.NBuckets),
		Data: map[string]any{"storage": c.Storage, "rho": c.Rho, "best_lag_buckets": c.BestLag, "n_buckets": c.NBuckets},
	}
}

func sampleEvidence(views []*view) result.Evidence {
	parts := make([]string, 0, len(views))
	for _, v := range views {
		parts = append(parts, fmt.Sprintf("%s %d ops", v.runner.Name, v.runner.Summary.N))
	}
	return result.Evidence{
		Kind: "sample_size",
		Text: fmt.Sprintf("%s; %.0f%% of measured buckets held enough samples",
			strings.Join(parts, ", "), sufficiency(views)*100),
	}
}

func signalsFor(incidents []result.Incident, runner string) []result.TelemetrySignal {
	seen := map[string]bool{}
	var out []result.TelemetrySignal
	for _, inc := range incidents {
		for _, s := range inc.Telemetry {
			key := s.Source + "/" + s.Signal
			if runnerForSource(s.Source) != runner || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, s)
		}
	}
	return out
}

func corroborationPhrase(incidents []result.Incident, runner string) string {
	sigs := signalsFor(incidents, runner)
	if len(sigs) == 0 {
		return " No server-side telemetry corroborated it."
	}
	names := make([]string, 0, len(sigs))
	for _, s := range sigs {
		names = append(names, s.Source+" "+s.Signal)
	}
	return fmt.Sprintf(" Server-side telemetry corroborated it: %s moved in the same window.", joinAnd(names))
}

func leadPhrase(incidents []result.Incident, runner string) string {
	var leads []int
	for _, inc := range incidents {
		if inc.Culprit != nil && inc.Culprit.Runner == runner {
			leads = append(leads, inc.Culprit.LeadBuckets)
		}
	}
	if len(leads) == 0 {
		return "went hot at the same time"
	}
	sort.Ints(leads)
	m := leads[len(leads)/2]
	switch {
	case m > 0:
		return fmt.Sprintf("went hot first, by %s", plural(m, "bucket"))
	case m == 0:
		return "went hot in the same bucket"
	default:
		return fmt.Sprintf("went hot %s after the application", plural(-m, "bucket"))
	}
}

// topIncidents returns the widest incidents first, earliest first on a tie.
func topIncidents(in []result.Incident, n int) []result.Incident {
	out := append([]result.Incident(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		wi, wj := out[i].EndIndex-out[i].StartIndex, out[j].EndIndex-out[j].StartIndex
		if wi != wj {
			return wi > wj
		}
		return out[i].StartIndex < out[j].StartIndex
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func countImpacting(in []result.Incident) int {
	n := 0
	for _, inc := range in {
		if inc.Class != result.IncidentStorageOnly {
			n++
		}
	}
	return n
}

func incidentSeconds(in []result.Incident) float64 {
	var s float64
	for _, inc := range in {
		s += inc.DurationS
	}
	return s
}

func tierName(runner string) string {
	switch runner {
	case "db":
		return "database tier"
	case "redis":
		return "cache tier (Redis)"
	default:
		return runner + " tier"
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// shareOf says how many of the incidents that reached users support the answer.
func shareOf(n, total int) string {
	switch {
	case n == 1 && total == 1:
		return "In the one incident that reached users"
	case n == total:
		return fmt.Sprintf("In all %d incidents that reached users", n)
	default:
		return fmt.Sprintf("In %d of the %d incidents that reached users", n, total)
	}
}

func seconds(s float64) string {
	if s == math.Trunc(s) {
		return fmt.Sprintf("%.0fs", s)
	}
	return fmt.Sprintf("%.1fs", s)
}

func ms(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0fms", v)
	}
	return fmt.Sprintf("%.1fms", v)
}

func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.3g", v)
}

func sloValue(metric string, v float64) string {
	if metric == "error_rate" {
		return fmt.Sprintf("%.2f%%", v*100)
	}
	return ms(v)
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// detailMap passes a finding's detail through as evidence data only when it is the
// object the schema expects.
func detailMap(d any) any {
	if m, ok := d.(map[string]any); ok {
		return m
	}
	return nil
}
