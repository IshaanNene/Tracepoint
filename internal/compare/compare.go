package compare

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/DataDog/sketches-go/ddsketch"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// SchemaVersion is the version of the comparison document's shape.
const SchemaVersion = "1.0"

// Changes, as the confidence intervals call them.
const (
	ChangeSlower       = "slower"
	ChangeFaster       = "faster"
	ChangeUnchanged    = "unchanged"
	ChangeInsufficient = "insufficient"
)

// Gates.
const (
	GateBudget    = "budget_crossing"
	GateRelative  = "relative_increase"
	GateErrorRate = "error_rate_increase"
)

// Incident statuses.
const (
	StatusNew       = "new"
	StatusFixed     = "fixed"
	StatusWorsened  = "worsened"
	StatusImproved  = "improved"
	StatusUnchanged = "unchanged"
)

// Finding codes a comparison can carry.
const (
	CodeConfigDiffers = "COMPARE_CONFIG_DIFFERS"
	CodeInvalidInput  = "COMPARE_INVALID_INPUT"
	CodeRunnerMissing = "COMPARE_RUNNER_MISSING"
)

// Z is the normal quantile of the confidence intervals: 95%, two-sided.
const Z = 1.96

// Quantile is the quantile the intervals are formed for.
const Quantile = 0.99

// Gates are the thresholds of a regression (spec §5.7). Zero fields take the
// defaults.
type Gates struct {
	// RelativePct and RelativeMS: a p99 rise counts only when it exceeds both, and
	// is significant.
	RelativePct float64
	RelativeMS  float64
	// ErrorPP is the error-rate rise, in percentage points, that fails.
	ErrorPP float64
	// Budgets are p99 budgets in milliseconds by runner. Unset runners take the
	// current run's SLO, else the baseline's.
	Budgets map[string]float64
}

// DefaultGates are the spec's.
func DefaultGates() Gates { return Gates{RelativePct: 10, RelativeMS: 5, ErrorPP: 0.5} }

func (g Gates) withDefaults() Gates {
	d := DefaultGates()
	if g.RelativePct <= 0 {
		g.RelativePct = d.RelativePct
	}
	if g.RelativeMS <= 0 {
		g.RelativeMS = d.RelativeMS
	}
	if g.ErrorPP <= 0 {
		g.ErrorPP = d.ErrorPP
	}
	return g
}

// Options configure a comparison.
type Options struct {
	Gates Gates
}

// Report is a comparison of two runs. Like every renderer's input it is a pure
// function of the two result documents.
type Report struct {
	SchemaVersion string           `json:"schema_version"`
	Baseline      RunRef           `json:"baseline"`
	Current       RunRef           `json:"current"`
	Regression    bool             `json:"regression"`
	Summary       string           `json:"summary"`
	Gates         []Gate           `json:"gates"`
	Runners       []Delta          `json:"runners"`
	Labels        []Delta          `json:"labels,omitempty"`
	Incidents     []IncidentDiff   `json:"incidents,omitempty"`
	ConfigDiff    []string         `json:"config_diff,omitempty"`
	Warnings      []result.Finding `json:"warnings,omitempty"`
	GateSettings  GateSettings     `json:"gate_settings"`
}

// GateSettings records the thresholds a comparison was judged by.
type GateSettings struct {
	RelativePct float64            `json:"relative_pct"`
	RelativeMS  float64            `json:"relative_ms"`
	ErrorPP     float64            `json:"error_pp"`
	BudgetsMS   map[string]float64 `json:"budgets_ms,omitempty"`
}

// RunRef identifies one side.
type RunRef struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	StartedAt     string  `json:"started_at,omitempty"`
	SchemaVersion string  `json:"schema_version"`
	Validity      string  `json:"validity"`
	DurationS     float64 `json:"duration_s"`
}

// Stats are one side's figures for a runner or label. Latencies are response time.
type Stats struct {
	N           int64   `json:"n"`
	P50MS       float64 `json:"p50_ms"`
	P95MS       float64 `json:"p95_ms"`
	P99MS       float64 `json:"p99_ms"`
	ErrorRatio  float64 `json:"error_ratio"`
	AchievedRPS float64 `json:"achieved_rps"`
}

// Delta is how a runner or label changed. Differences are current minus baseline.
type Delta struct {
	Runner   string  `json:"runner"`
	Label    string  `json:"label,omitempty"`
	Baseline Stats   `json:"baseline"`
	Current  Stats   `json:"current"`
	P50MS    float64 `json:"p50_delta_ms"`
	P95MS    float64 `json:"p95_delta_ms"`
	P99MS    float64 `json:"p99_delta_ms"`
	P99Pct   float64 `json:"p99_delta_pct"`
	ErrorPP  float64 `json:"error_delta_pp"`
	RPSPct   float64 `json:"rps_delta_pct"`
	P99CI    CIPair  `json:"p99_ci"`
	// Change is what the intervals say: slower or faster only when they do not
	// overlap, insufficient when either could not be formed.
	Change string `json:"change"`
}

// CIPair holds each side's 95% interval for p99, nil where it could not be formed.
type CIPair struct {
	Baseline []float64 `json:"baseline"`
	Current  []float64 `json:"current"`
}

// Gate is one gate's outcome for one runner.
type Gate struct {
	Name   string `json:"gate"`
	Runner string `json:"runner"`
	Pass   bool   `json:"pass"`
	// Insufficient marks a failure that is a lack of evidence: a rise large enough
	// to cross the gate that the data cannot call real or clear.
	Insufficient bool    `json:"insufficient,omitempty"`
	Threshold    float64 `json:"threshold"`
	Baseline     float64 `json:"baseline"`
	Current      float64 `json:"current"`
	Message      string  `json:"message"`
}

// IncidentDiff pairs incidents across the runs by runner and time.
type IncidentDiff struct {
	Runner   string       `json:"runner"`
	Status   string       `json:"status"`
	Baseline *IncidentRef `json:"baseline"`
	Current  *IncidentRef `json:"current"`
}

// IncidentRef is one side's incident.
type IncidentRef struct {
	ID        string  `json:"id"`
	Class     string  `json:"class"`
	StartS    float64 `json:"start_s"`
	EndS      float64 `json:"end_s"`
	PeakP99MS float64 `json:"peak_p99_ms"`
}

// Compare diffs two runs (spec §5.7). It fails only when the two documents cannot be
// compared at all; everything short of that is a warning in the report.
func Compare(baseline, current *result.Result, opts Options) (*Report, error) {
	if major(baseline.SchemaVersion) != major(current.SchemaVersion) {
		return nil, errs.New(errs.CodeCompareSchemaMismatch,
			"the baseline is result schema %s and the current run %s; their major versions differ",
			baseline.SchemaVersion, current.SchemaVersion).
			WithHint("re-run the baseline with this TracePoint, or compare with the version that wrote both")
	}
	g := opts.Gates.withDefaults()
	rep := &Report{
		SchemaVersion: SchemaVersion,
		Baseline:      ref(baseline), Current: ref(current),
		Gates:        []Gate{},
		Runners:      []Delta{},
		GateSettings: GateSettings{RelativePct: g.RelativePct, RelativeMS: g.RelativeMS, ErrorPP: g.ErrorPP},
	}

	for _, side := range []struct {
		name string
		r    *result.Result
	}{{"baseline", baseline}, {"current", current}} {
		if side.r.Analysis.Validity.State == result.ValidityInvalid {
			rep.Warnings = append(rep.Warnings, result.Finding{
				Code: CodeInvalidInput, Severity: result.SeverityError,
				Message: fmt.Sprintf("the %s run (%s) is invalid: the generator, not the target, set its pace, so differences may be the generator's", side.name, side.r.Run.ID),
				Fix:     "re-run it until it is valid before trusting this comparison",
			})
		}
	}

	rep.ConfigDiff = configDiff(baseline.Config, current.Config)
	if len(rep.ConfigDiff) > 0 {
		rep.Warnings = append(rep.Warnings, result.Finding{
			Code: CodeConfigDiffers, Severity: result.SeverityWarn,
			Message: fmt.Sprintf("the runs were configured differently: %s", strings.Join(rep.ConfigDiff, ", ")),
			Fix:     "a change may come from the configuration rather than the system; compare runs of one configuration",
		})
	}

	budgets := budgetsFor(baseline, current, g.Budgets)
	if len(budgets) > 0 {
		rep.GateSettings.BudgetsMS = budgets
	}
	for _, name := range runnerNames(baseline, current) {
		b, c := baseline.RunnerByName(name), current.RunnerByName(name)
		if b == nil || c == nil {
			which := "baseline"
			if c == nil {
				which = "current run"
			}
			rep.Warnings = append(rep.Warnings, result.Finding{
				Code: CodeRunnerMissing, Severity: result.SeverityWarn,
				Message: fmt.Sprintf("the %s runner is missing from the %s, so it is not compared", name, which),
			})
			continue
		}
		d := diff(name, "", b.Summary, c.Summary, b.Sketches[metrics.RunnerSketchKey], c.Sketches[metrics.RunnerSketchKey])
		rep.Runners = append(rep.Runners, d)
		rep.Gates = append(rep.Gates, gates(d, g, budgets)...)

		labels := make([]string, 0, len(c.LabelSummaries))
		for l := range c.LabelSummaries {
			if _, ok := b.LabelSummaries[l]; ok {
				labels = append(labels, l)
			}
		}
		sort.Strings(labels)
		for _, l := range labels {
			rep.Labels = append(rep.Labels, diff(name, l, b.LabelSummaries[l], c.LabelSummaries[l], b.Sketches[l], c.Sketches[l]))
		}
	}

	rep.Incidents = incidentDiff(baseline, current, g)
	for _, gt := range rep.Gates {
		if !gt.Pass {
			rep.Regression = true
		}
	}
	rep.Summary = summarise(rep)
	return rep, nil
}

// ExitCode is what a CI job should exit with: 1 when a gate failed.
func (r *Report) ExitCode() int {
	if r.Regression {
		return errs.ExitBreach
	}
	return errs.ExitOK
}

// ErrorCode names why the comparison failed, or is empty when it did not: a
// regression, or - when every failing gate lacked the data to decide - insufficient
// data.
func (r *Report) ErrorCode() errs.Code {
	if !r.Regression {
		return ""
	}
	for _, g := range r.Gates {
		if !g.Pass && !g.Insufficient {
			return errs.CodeCompareRegression
		}
	}
	return errs.CodeCompareInsufficient
}

func ref(r *result.Result) RunRef {
	d := r.Run.ElapsedMS
	if d == 0 {
		d = r.Run.DurationMS
	}
	return RunRef{ID: r.Run.ID, Name: r.Run.Name, StartedAt: r.Run.StartedAt, SchemaVersion: r.SchemaVersion,
		Validity: r.Analysis.Validity.State, DurationS: round(d/1000, 3)}
}

func stats(s result.OpSummary) Stats {
	return Stats{N: s.N, P50MS: round(s.Response.P50, 3), P95MS: round(s.Response.P95, 3), P99MS: round(s.Response.P99, 3),
		ErrorRatio: round(s.ErrorRatio, 6), AchievedRPS: round(s.AchievedRPS, 3)}
}

func diff(runner, label string, b, c result.OpSummary, bs, cs result.Sketch) Delta {
	d := Delta{Runner: runner, Label: label, Baseline: stats(b), Current: stats(c)}
	d.P50MS = round(c.Response.P50-b.Response.P50, 3)
	d.P95MS = round(c.Response.P95-b.Response.P95, 3)
	d.P99MS = round(c.Response.P99-b.Response.P99, 3)
	if b.Response.P99 > 0 {
		d.P99Pct = round(100*(c.Response.P99-b.Response.P99)/b.Response.P99, 2)
	}
	d.ErrorPP = round(100*(c.ErrorRatio-b.ErrorRatio), 4)
	if b.AchievedRPS > 0 {
		d.RPSPct = round(100*(c.AchievedRPS-b.AchievedRPS)/b.AchievedRPS, 2)
	}

	bLo, bHi, bOK := intervalOf(bs)
	cLo, cHi, cOK := intervalOf(cs)
	if bOK {
		d.P99CI.Baseline = []float64{round(bLo, 3), round(bHi, 3)}
	}
	if cOK {
		d.P99CI.Current = []float64{round(cLo, 3), round(cHi, 3)}
	}
	switch {
	case !bOK || !cOK:
		d.Change = ChangeInsufficient
	case cLo > bHi:
		d.Change = ChangeSlower
	case cHi < bLo:
		d.Change = ChangeFaster
	default:
		d.Change = ChangeUnchanged
	}
	return d
}

func intervalOf(s result.Sketch) (lo, hi float64, ok bool) {
	if s.Data == "" {
		return 0, 0, false
	}
	sk, err := metrics.DecodeSketch(metrics.EncodedSketch{Encoding: s.Encoding, RelativeAccuracy: s.RelativeAccuracy, Count: s.Count, Data: s.Data})
	if err != nil {
		return 0, 0, false
	}
	return quantileCI(sk, Quantile)
}

// quantileCI is the distribution-free interval for a quantile: the order statistics at
// ranks n·q ± Z·√(n·q·(1−q)), read from the sketch (spec §5.7). A rank outside
// [1, n] means there is not enough data for an interval, and ok is false.
func quantileCI(s *ddsketch.DDSketch, q float64) (lo, hi float64, ok bool) {
	n := s.GetCount()
	if n < 2 {
		return 0, 0, false
	}
	c, h := n*q, Z*math.Sqrt(n*q*(1-q))
	rlo, rhi := math.Floor(c-h), math.Ceil(c+h)
	if rlo < 1 || rhi > n {
		return 0, 0, false
	}
	at := func(rank float64) float64 {
		v, err := s.GetValueAtQuantile((rank - 1) / (n - 1))
		if err != nil {
			return math.NaN()
		}
		return v
	}
	lo, hi = at(rlo), at(rhi)
	if math.IsNaN(lo) || math.IsNaN(hi) {
		return 0, 0, false
	}
	return lo, hi, true
}

// gates judges one runner's delta.
func gates(d Delta, g Gates, budgets map[string]float64) []Gate {
	var out []Gate

	rise := d.P99Pct > g.RelativePct && d.P99MS > g.RelativeMS
	rel := Gate{Name: GateRelative, Runner: d.Runner, Pass: true, Threshold: g.RelativePct,
		Baseline: d.Baseline.P99MS, Current: d.Current.P99MS}
	switch {
	case rise && d.Change == ChangeSlower:
		rel.Pass = false
		rel.Message = fmt.Sprintf("%s p99 rose %.1f%% (+%.1fms), %.0fms to %.0fms, and the intervals do not overlap",
			d.Runner, d.P99Pct, d.P99MS, d.Baseline.P99MS, d.Current.P99MS)
	case rise && d.Change == ChangeInsufficient:
		rel.Pass, rel.Insufficient = false, true
		rel.Message = fmt.Sprintf("%s p99 rose %.1f%% (+%.1fms), but there are too few samples to tell whether that is real",
			d.Runner, d.P99Pct, d.P99MS)
	case rise:
		rel.Message = fmt.Sprintf("%s p99 rose %.1f%% (+%.1fms), within what sampling alone explains", d.Runner, d.P99Pct, d.P99MS)
	default:
		rel.Message = fmt.Sprintf("%s p99 %.0fms to %.0fms: under %.0f%% and %.0fms", d.Runner, d.Baseline.P99MS, d.Current.P99MS, g.RelativePct, g.RelativeMS)
	}
	out = append(out, rel)

	if budget, ok := budgets[d.Runner]; ok {
		b := Gate{Name: GateBudget, Runner: d.Runner, Pass: true, Threshold: budget, Baseline: d.Baseline.P99MS, Current: d.Current.P99MS}
		if d.Current.P99MS > budget && d.Baseline.P99MS <= budget {
			b.Pass = false
			b.Message = fmt.Sprintf("%s p99 crossed its %.0fms budget: %.0fms to %.0fms", d.Runner, budget, d.Baseline.P99MS, d.Current.P99MS)
		} else {
			b.Message = fmt.Sprintf("%s p99 did not cross its %.0fms budget", d.Runner, budget)
		}
		out = append(out, b)
	}

	e := Gate{Name: GateErrorRate, Runner: d.Runner, Pass: d.ErrorPP <= g.ErrorPP, Threshold: g.ErrorPP,
		Baseline: d.Baseline.ErrorRatio, Current: d.Current.ErrorRatio}
	e.Message = fmt.Sprintf("%s error rate %.2f%% to %.2f%% (%+.2f points)", d.Runner, 100*d.Baseline.ErrorRatio, 100*d.Current.ErrorRatio, d.ErrorPP)
	out = append(out, e)
	return out
}

// budgetsFor takes the explicit budgets first, then the current run's SLO, then the
// baseline's.
func budgetsFor(b, c *result.Result, explicit map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, r := range []*result.Result{b, c} {
		for _, ch := range r.Analysis.SLO.Checks {
			if ch.Metric == "p99" {
				out[ch.Runner] = ch.Budget
			}
		}
	}
	for k, v := range explicit {
		out[k] = v
	}
	return out
}

func runnerNames(b, c *result.Result) []string {
	var names []string
	seen := map[string]bool{}
	for _, r := range []*result.Result{c, b} {
		for _, rn := range r.Runners {
			if !seen[rn.Name] {
				seen[rn.Name] = true
				names = append(names, rn.Name)
			}
		}
	}
	return names
}

// incidentDiff pairs each runner's incidents across the runs: two overlap when their
// windows meet within two buckets.
func incidentDiff(b, c *result.Result, g Gates) []IncidentDiff {
	tol := 2 * math.Max(b.Run.BucketMS, c.Run.BucketMS) / 1000
	var out []IncidentDiff
	for _, name := range runnerNames(b, c) {
		base, cur := hotIn(b, name), hotIn(c, name)
		used := make([]bool, len(base))
		for i := range cur {
			ci := cur[i]
			match := -1
			for j, bi := range base {
				if !used[j] && bi.StartS-tol <= ci.EndS && ci.StartS <= bi.EndS+tol {
					match = j
					break
				}
			}
			if match < 0 {
				out = append(out, IncidentDiff{Runner: name, Status: StatusNew, Current: &ci})
				continue
			}
			used[match] = true
			bi := base[match]
			status := StatusUnchanged
			switch {
			case ci.PeakP99MS > bi.PeakP99MS*(1+g.RelativePct/100) && ci.PeakP99MS-bi.PeakP99MS > g.RelativeMS:
				status = StatusWorsened
			case bi.PeakP99MS > ci.PeakP99MS*(1+g.RelativePct/100) && bi.PeakP99MS-ci.PeakP99MS > g.RelativeMS:
				status = StatusImproved
			}
			out = append(out, IncidentDiff{Runner: name, Status: status, Baseline: &bi, Current: &ci})
		}
		for j := range base {
			if !used[j] {
				bi := base[j]
				out = append(out, IncidentDiff{Runner: name, Status: StatusFixed, Baseline: &bi})
			}
		}
	}
	return out
}

func hotIn(r *result.Result, runner string) []IncidentRef {
	var out []IncidentRef
	for _, inc := range r.Analysis.Incidents {
		for _, ir := range inc.Runners {
			if ir.Name == runner && ir.Hot {
				out = append(out, IncidentRef{ID: inc.ID, Class: inc.Class, StartS: inc.StartS, EndS: inc.EndS, PeakP99MS: ir.PeakP99MS})
			}
		}
	}
	return out
}

// configDiff lists every key whose value differs between the effective configurations.
func configDiff(b, c *result.Config) []string {
	if b == nil || c == nil {
		return nil
	}
	fb, fc := map[string]string{}, map[string]string{}
	flatten("", normalise(b.Effective), fb)
	flatten("", normalise(c.Effective), fc)
	var out []string
	for k, v := range fb {
		if w, ok := fc[k]; !ok || w != v {
			out = append(out, k)
		}
	}
	for k := range fc {
		if _, ok := fb[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// normalise turns any value into plain JSON types, so a configuration held as a Go
// struct and one read back from a file flatten the same way.
func normalise(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func flatten(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, sub, out)
		}
	default:
		raw, err := json.Marshal(t)
		if err == nil && prefix != "" {
			out[prefix] = string(raw)
		}
	}
}

func summarise(r *Report) string {
	var failed []string
	insufficient := true
	for _, g := range r.Gates {
		if !g.Pass {
			failed = append(failed, g.Message)
			insufficient = insufficient && g.Insufficient
		}
	}
	var changed []string
	for _, d := range r.Runners {
		if d.Change == ChangeSlower || d.Change == ChangeFaster {
			changed = append(changed, fmt.Sprintf("%s p99 %s (%+.1f%%)", d.Runner, d.Change, d.P99Pct))
		}
	}
	var s string
	switch {
	case len(failed) > 0 && insufficient:
		s = "Cannot rule out a regression: " + strings.Join(failed, "; ") + "."
	case len(failed) > 0:
		s = "Regression: " + strings.Join(failed, "; ") + "."
	case len(changed) > 0:
		s = "No gate crossed. Significant changes: " + strings.Join(changed, "; ") + "."
	default:
		s = "No significant change: every p99 interval overlaps its baseline's, and no gate was crossed."
	}
	for _, w := range r.Warnings {
		if w.Severity == result.SeverityError {
			s += " " + w.Message + "."
		}
	}
	return s
}

func major(v string) string {
	if i := strings.IndexByte(v, '.'); i >= 0 {
		return v[:i]
	}
	return v
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
