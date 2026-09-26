package result

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// DigestSchemaVersion is the version of the digest document's shape.
const DigestSchemaVersion = "1.1"

// DefaultDigestBudget is the character budget a digest is cut to when none is given:
// small enough to read in full inside a context window, large enough to carry a
// verdict with its evidence.
const DefaultDigestBudget = 4000

// Digest is the agent-facing view of a run (§6.3): prioritised, compact and in a
// fixed order - validity, SLO, verdict, incidents, runners, recommendations,
// artifacts - so a reader can stop as soon as it has its answer. It mirrors
// schemas/digest.schema.json and, like every renderer, is a pure function of the
// result document.
type Digest struct {
	SchemaVersion   string              `json:"schema_version"`
	RunID           string              `json:"run_id"`
	RunName         string              `json:"run_name,omitempty"`
	Status          string              `json:"status"`
	StartedAt       string              `json:"started_at,omitempty"`
	DurationS       float64             `json:"duration_s,omitempty"`
	Validity        DigestValidity      `json:"validity"`
	Caveats         []DigestFinding     `json:"caveats,omitempty"`
	SLO             DigestSLO           `json:"slo"`
	Verdict         DigestVerdict       `json:"verdict"`
	Incidents       []DigestIncident    `json:"incidents,omitempty"`
	Runners         []DigestRunner      `json:"runners,omitempty"`
	Correlation     []DigestCorrelation `json:"correlation,omitempty"`
	Telemetry       []DigestTelemetry   `json:"telemetry,omitempty"`
	Strain          *DigestStrain       `json:"strain,omitempty"`
	Recommendations []Recommendation    `json:"recommendations,omitempty"`
	Artifacts       *DigestArtifacts    `json:"artifacts,omitempty"`
	Truncated       bool                `json:"truncated"`
	More            *DigestMore         `json:"more,omitempty"`
}

// DigestStrain is where latency started to degrade on a ramping run, and the range a
// follow-up capacity search should cover.
type DigestStrain struct {
	Found      bool    `json:"found"`
	Users      float64 `json:"users"`
	RPS        float64 `json:"rps"`
	Message    string  `json:"message"`
	NextWindow any     `json:"next_window,omitempty"`
}

// DigestValidity is whether the run can be believed.
type DigestValidity struct {
	State    string          `json:"state"`
	Findings []DigestFinding `json:"findings,omitempty"`
}

// DigestFinding is one validity finding, with its fix.
type DigestFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

// DigestSLO is the budget outcome.
type DigestSLO struct {
	Pass       bool           `json:"pass"`
	Configured bool           `json:"configured"`
	Breaches   []DigestBreach `json:"breaches,omitempty"`
}

// DigestBreach is one missed budget.
type DigestBreach struct {
	Runner string  `json:"runner"`
	Metric string  `json:"metric"`
	Budget float64 `json:"budget"`
	Actual float64 `json:"actual"`
}

// DigestVerdict is the answer, with its evidence as plain sentences.
type DigestVerdict struct {
	Bottleneck string   `json:"bottleneck"`
	Confidence string   `json:"confidence"`
	Summary    string   `json:"summary"`
	Evidence   []string `json:"evidence,omitempty"`
	Caveat     string   `json:"caveat,omitempty"`
	NextSteps  []string `json:"next_steps,omitempty"`
}

// DigestIncident is one episode, flattened.
type DigestIncident struct {
	ID            string   `json:"id"`
	Class         string   `json:"class"`
	StartS        float64  `json:"start_s"`
	DurationS     float64  `json:"duration_s"`
	RunnersHot    []string `json:"runners_hot,omitempty"`
	Culprit       *string  `json:"culprit"`
	AppP99MS      float64  `json:"app_p99_ms,omitempty"`
	CulpritP99MS  float64  `json:"culprit_p99_ms,omitempty"`
	LeadBuckets   int      `json:"lead_buckets,omitempty"`
	Corroboration []string `json:"corroboration,omitempty"`
}

// DigestRunner is one tier's headline numbers.
type DigestRunner struct {
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	N               int64    `json:"n"`
	AchievedRPS     float64  `json:"achieved_rps"`
	ErrorRatio      float64  `json:"error_ratio"`
	P50MS           float64  `json:"p50_ms"`
	P95MS           float64  `json:"p95_ms"`
	P99MS           float64  `json:"p99_ms"`
	ServiceP99MS    float64  `json:"service_p99_ms"`
	ClientWaitP99MS float64  `json:"client_wait_p99_ms"`
	DroppedRatio    float64  `json:"dropped_ratio"`
	TopErrors       []string `json:"top_errors,omitempty"`
}

// DigestCorrelation is one storage probe's whole-run correlation with the application.
type DigestCorrelation struct {
	Storage    string  `json:"storage"`
	Rho        float64 `json:"rho"`
	LagBuckets int     `json:"lag_buckets"`
	NBuckets   int     `json:"n_buckets,omitempty"`
}

// DigestTelemetry is one sampler in a line.
type DigestTelemetry struct {
	Source     string   `json:"source"`
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
	Highlights []string `json:"highlights,omitempty"`
}

// Recommendation is a machine-applicable next step. One with action rerun can be
// executed directly: `tracepoint run --from <run_id>` plus one --set per override.
type Recommendation struct {
	ID        string   `json:"id"`
	Why       string   `json:"why"`
	Action    string   `json:"action"`
	Overrides []string `json:"overrides,omitempty"`
	Priority  int      `json:"priority,omitempty"`
}

// Recommendation actions.
const (
	ActionRerun       = "rerun"
	ActionConfigure   = "configure"
	ActionInvestigate = "investigate"
)

// DigestArtifacts says where the full data lives.
type DigestArtifacts struct {
	RunDir string `json:"run_dir,omitempty"`
	Result string `json:"result,omitempty"`
	Report string `json:"report,omitempty"`
	Events string `json:"events,omitempty"`
}

// DigestMore says how to get what a truncated digest left out.
type DigestMore struct {
	Hint     string   `json:"hint"`
	Sections []string `json:"sections,omitempty"`
	Dropped  []string `json:"dropped,omitempty"`
}

// DigestOptions control the digest.
type DigestOptions struct {
	// BudgetChars caps the size of the encoding Encode writes. Zero means
	// DefaultDigestBudget; negative means no limit.
	BudgetChars int
	// Ref is how a follow-up command should name this run - its id or its result
	// path - for the hint in a truncated digest.
	Ref string
}

// BuildDigest reduces a result to its digest, then drops the lowest-priority content
// until the encoding fits the budget.
func BuildDigest(r *Result, opts DigestOptions) *Digest {
	d := fullDigest(r)
	budget := opts.BudgetChars
	if budget == 0 {
		budget = DefaultDigestBudget
	}
	if budget > 0 {
		fit(d, budget, opts.Ref, r.RunID())
	}
	return d
}

// RunID is the run's identifier.
func (r *Result) RunID() string { return r.Run.ID }

// Encode writes the digest as indented JSON with a trailing newline.
func (d *Digest) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "encoding the digest")
	}
	return append(b, '\n'), nil
}

// size is the length of the encoding Encode writes, which is what the budget limits:
// a budget measured on some other encoding would not hold for what a reader receives.
func (d *Digest) size() int {
	b, err := d.Encode()
	if err != nil {
		return math.MaxInt
	}
	return len(b)
}

func fullDigest(r *Result) *Digest {
	a := &r.Analysis
	d := &Digest{
		SchemaVersion: DigestSchemaVersion,
		RunID:         r.Run.ID,
		RunName:       r.Run.Name,
		Status:        r.Run.Status,
		StartedAt:     r.Run.StartedAt,
		DurationS:     roundTo(r.Run.DurationMS/1000, 3),
		Validity:      DigestValidity{State: a.Validity.State},
		SLO:           DigestSLO{Pass: a.SLO.Pass, Configured: len(a.SLO.Checks) > 0},
		Verdict: DigestVerdict{
			Bottleneck: a.Verdict.Bottleneck, Confidence: a.Verdict.Confidence,
			Summary: a.Verdict.Summary, Caveat: a.Verdict.Caveat, NextSteps: a.Verdict.NextSteps,
		},
	}
	if r.Run.ElapsedMS > 0 {
		d.DurationS = roundTo(r.Run.ElapsedMS/1000, 3)
	}
	for _, f := range a.Validity.Findings {
		d.Validity.Findings = append(d.Validity.Findings, DigestFinding{
			Code: f.Code, Severity: f.Severity, Message: f.Message, Fix: f.Fix,
		})
	}
	for _, c := range a.SLO.Checks {
		if !c.Pass {
			d.SLO.Breaches = append(d.SLO.Breaches, DigestBreach{Runner: c.Runner, Metric: c.Metric, Budget: c.Budget, Actual: c.Actual})
		}
	}
	for _, e := range a.Verdict.Evidence {
		d.Verdict.Evidence = append(d.Verdict.Evidence, e.Text)
	}

	for _, f := range r.Warnings {
		d.Caveats = append(d.Caveats, DigestFinding{Code: f.Code, Severity: f.Severity, Message: f.Message, Fix: f.Fix})
	}

	for _, inc := range worstFirst(a.Incidents) {
		d.Incidents = append(d.Incidents, digestIncident(inc))
	}

	for i := range r.Runners {
		d.Runners = append(d.Runners, digestRunner(&r.Runners[i]))
	}
	for _, c := range a.Correlation {
		d.Correlation = append(d.Correlation, DigestCorrelation{Storage: c.Storage, Rho: c.Rho, LagBuckets: c.BestLag, NBuckets: c.NBuckets})
	}
	d.Telemetry = digestTelemetry(r)
	if s := a.Strain; s != nil {
		d.Strain = &DigestStrain{Found: s.Found, Users: s.Users, RPS: s.RPS, Message: s.Message, NextWindow: s.NextWindow}
	}
	d.Recommendations = Recommend(r)

	if art := r.Artifacts; art != nil {
		d.Artifacts = &DigestArtifacts{RunDir: art.RunDir, Result: art.Result, Report: art.Report, Events: art.Events}
		if *d.Artifacts == (DigestArtifacts{}) {
			d.Artifacts = nil
		}
	}
	return d
}

// worstFirst orders incidents by how much they matter: those that reached users before
// those that did not, then by peak severity times duration, then by start.
func worstFirst(in []Incident) []Incident {
	out := append([]Incident(nil), in...)
	weight := func(inc Incident) float64 {
		peak := 0.0
		for _, rn := range inc.Runners {
			if rn.Hot && rn.Severity > peak {
				peak = rn.Severity
			}
		}
		return peak * float64(inc.EndIndex-inc.StartIndex+1)
	}
	reached := func(inc Incident) bool { return inc.Class != IncidentStorageOnly }
	sort.SliceStable(out, func(i, j int) bool {
		if reached(out[i]) != reached(out[j]) {
			return reached(out[i])
		}
		if wi, wj := weight(out[i]), weight(out[j]); wi != wj {
			return wi > wj
		}
		return out[i].StartIndex < out[j].StartIndex
	})
	return out
}

func digestIncident(inc Incident) DigestIncident {
	d := DigestIncident{ID: inc.ID, Class: inc.Class, StartS: inc.StartS, DurationS: inc.DurationS}
	for _, rn := range inc.Runners {
		if rn.Hot {
			d.RunnersHot = append(d.RunnersHot, rn.Name)
		}
	}
	if inc.AppImpact != nil {
		d.AppP99MS = inc.AppImpact.PeakP99MS
	}
	// A tie names no culprit in the digest: the schema says null means none stands
	// out or several tie, and a reader should not have to inspect tied_with to learn
	// that the first name is arbitrary.
	if c := inc.Culprit; c != nil && len(c.TiedWith) == 0 {
		name := c.Runner
		d.Culprit = &name
		d.LeadBuckets = c.LeadBuckets
		for _, rn := range inc.Runners {
			if rn.Name == c.Runner {
				d.CulpritP99MS = rn.PeakP99MS
			}
		}
	}
	for _, s := range inc.Telemetry {
		d.Corroboration = append(d.Corroboration,
			fmt.Sprintf("%s %s moved from %s to %s", s.Source, s.Signal, compact(s.Baseline), compact(s.Value)))
	}
	return d
}

func digestRunner(rn *Runner) DigestRunner {
	s := rn.Summary
	d := DigestRunner{
		Name: rn.Name, Kind: rn.Kind, N: s.N,
		AchievedRPS: s.AchievedRPS, ErrorRatio: roundTo(s.ErrorRatio, 6),
		P50MS: s.Response.P50, P95MS: s.Response.P95, P99MS: s.Response.P99,
		ServiceP99MS: s.Service.P99, ClientWaitP99MS: s.ClientWait.P99,
	}
	if rn.Executor != nil {
		d.DroppedRatio = roundTo(rn.Executor.DroppedRatio, 6)
	}
	type kv struct {
		k string
		v int64
	}
	var errsByClass []kv
	for k, v := range s.Errors {
		if v > 0 {
			errsByClass = append(errsByClass, kv{k, v})
		}
	}
	sort.Slice(errsByClass, func(i, j int) bool {
		if errsByClass[i].v != errsByClass[j].v {
			return errsByClass[i].v > errsByClass[j].v
		}
		return errsByClass[i].k < errsByClass[j].k
	})
	for i, e := range errsByClass {
		if i == 3 {
			break
		}
		d.TopErrors = append(d.TopErrors, fmt.Sprintf("%s: %d", e.k, e.v))
	}
	return d
}

func digestTelemetry(r *Result) []DigestTelemetry {
	t := r.Telemetry
	if t == nil {
		return nil
	}
	var out []DigestTelemetry
	for _, src := range []struct {
		name string
		s    *SamplerSeries
	}{{"postgres", t.Postgres}, {"mysql", t.MySQL}, {"redis", t.Redis}} {
		if src.s == nil {
			continue
		}
		dt := DigestTelemetry{Source: src.name, Available: src.s.Available, Reason: src.s.Reason}
		seen := map[string]bool{}
		for _, inc := range r.Analysis.Incidents {
			for _, sig := range inc.Telemetry {
				if sig.Source != src.name || seen[sig.Signal] {
					continue
				}
				seen[sig.Signal] = true
				dt.Highlights = append(dt.Highlights, fmt.Sprintf("%s moved from %s to %s during %s",
					sig.Signal, compact(sig.Baseline), compact(sig.Value), inc.ID))
			}
		}
		if src.s.Available && len(dt.Highlights) == 0 && len(r.Analysis.Incidents) > 0 {
			dt.Highlights = append(dt.Highlights, "no signal moved during any incident")
		}
		out = append(out, dt)
	}
	if g := t.Generator; g != nil && len(g.Samples) > 0 {
		dt := DigestTelemetry{Source: "generator", Available: true}
		var cpu, sched float64
		for _, s := range g.Samples {
			if v, ok := numberOf(s["cpu_ratio"]); ok && v > cpu {
				cpu = v
			}
			if v, ok := numberOf(s["sched_latency_p99_ms"]); ok && v > sched {
				sched = v
			}
		}
		dt.Highlights = append(dt.Highlights,
			fmt.Sprintf("peak cpu_ratio %.2f", cpu), fmt.Sprintf("peak sched_latency_p99_ms %s", compact(sched)))
		out = append(out, dt)
	}
	return out
}

// fit drops content, lowest priority first, until the digest fits the budget. Each
// step names what it dropped so a reader knows what to ask for.
func fit(d *Digest, budget int, ref, runID string) {
	if d.size() <= budget {
		return
	}
	if ref == "" {
		ref = runID
	}
	var dropped []string
	steps := []struct {
		name string
		do   func() bool
	}{
		{"telemetry", func() bool { return drop(&d.Telemetry) }},
		{"correlation", func() bool { return drop(&d.Correlation) }},
		{"caveats", func() bool { return drop(&d.Caveats) }},
		{"strain", func() bool {
			if d.Strain == nil {
				return false
			}
			d.Strain = nil
			return true
		}},
		{"incidents beyond the top 3", func() bool { return trim(&d.Incidents, 3) }},
		{"verdict evidence beyond the first 3", func() bool { return trim(&d.Verdict.Evidence, 3) }},
		{"runner error breakdowns", func() bool {
			changed := false
			for i := range d.Runners {
				if d.Runners[i].TopErrors != nil {
					d.Runners[i].TopErrors, changed = nil, true
				}
			}
			return changed
		}},
		{"recommendations beyond the top 3", func() bool { return trim(&d.Recommendations, 3) }},
		{"incidents beyond the worst", func() bool { return trim(&d.Incidents, 1) }},
		{"next steps beyond the first 2", func() bool { return trim(&d.Verdict.NextSteps, 2) }},
		{"verdict evidence beyond the first", func() bool { return trim(&d.Verdict.Evidence, 1) }},
		{"storage runner summaries", func() bool {
			var keep []DigestRunner
			for _, rn := range d.Runners {
				if rn.Kind == "app" {
					keep = append(keep, rn)
				}
			}
			if len(keep) == len(d.Runners) {
				return false
			}
			d.Runners = keep
			return true
		}},
		{"recommendations beyond the first", func() bool { return trim(&d.Recommendations, 1) }},
		{"validity findings below error severity", func() bool {
			var keep []DigestFinding
			for _, f := range d.Validity.Findings {
				if f.Severity == SeverityError {
					keep = append(keep, f)
				}
			}
			if len(keep) == len(d.Validity.Findings) {
				return false
			}
			d.Validity.Findings = keep
			return true
		}},
		{"next steps", func() bool { return drop(&d.Verdict.NextSteps) }},
		// Below here only the required core is left - validity, SLO, the verdict with
		// its caveat, and where to find the rest - and that is never dropped: a
		// digest too small to hold it would not be worth reading.
		{"recommendations", func() bool { return drop(&d.Recommendations) }},
		{"runner summaries", func() bool { return drop(&d.Runners) }},
		{"incidents", func() bool { return drop(&d.Incidents) }},
		{"verdict evidence", func() bool { return drop(&d.Verdict.Evidence) }},
		{"validity findings", func() bool {
			// Keep the codes and fixes of anything that invalidates the run; the
			// messages are the part that can be fetched again.
			changed := false
			for i := range d.Validity.Findings {
				if d.Validity.Findings[i].Message != "" && d.Validity.Findings[i].Severity != SeverityError {
					d.Validity.Findings[i].Message = d.Validity.Findings[i].Code
					changed = true
				}
			}
			return changed
		}},
	}
	d.Truncated = true
	d.More = &DigestMore{
		Hint:     fmt.Sprintf("tracepoint digest %s --budget-chars %d, or get_run_section for one section at a time", ref, budget*4),
		Sections: []string{"incidents", "labels", "timeline", "telemetry"},
	}
	for _, s := range steps {
		if d.size() <= budget {
			break
		}
		if s.do() {
			dropped = append(dropped, s.name)
			d.More.Dropped = dropped
		}
	}
}

func drop[T any](s *[]T) bool {
	if len(*s) == 0 {
		return false
	}
	*s = nil
	return true
}

func trim[T any](s *[]T, n int) bool {
	if len(*s) <= n {
		return false
	}
	*s = (*s)[:n]
	return true
}

func roundTo(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// compact prints a number without trailing noise: integers as integers, others to
// three significant figures.
func compact(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3g", v), "0"), ".")
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}
