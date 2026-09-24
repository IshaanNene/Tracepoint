package analysis

import (
	"fmt"
	"math"
	"sort"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// span is an inclusive range of bucket indexes.
type span struct{ start, end int }

// episodes merges one runner's hot buckets across gaps of at most gap buckets.
func episodes(hot []result.HotBucket, gap int) []span {
	var out []span
	for _, h := range hot {
		if n := len(out); n > 0 && h.Index-out[n-1].end-1 <= gap {
			if h.Index > out[n-1].end {
				out[n-1].end = h.Index
			}
			continue
		}
		out = append(out, span{h.Index, h.Index})
	}
	return out
}

// view is one runner prepared for the incident logic.
type view struct {
	runner    *result.Runner
	app       bool
	threshold float64
	hot       []result.HotBucket
	hotSet    map[int]bool
	byIndex   map[int]*result.Bucket
	baseline  float64 // median service p99 of the runner's steady buckets
}

func newView(r *result.Runner, threshold float64, hot []result.HotBucket) *view {
	v := &view{
		runner: r, app: r.Kind == "app", threshold: threshold, hot: hot,
		hotSet:  make(map[int]bool, len(hot)),
		byIndex: make(map[int]*result.Bucket, len(r.Buckets)),
	}
	for _, h := range hot {
		v.hotSet[h.Index] = true
	}
	var calm []float64
	for i := range r.Buckets {
		b := &r.Buckets[i]
		v.byIndex[b.Index] = b
		if eligible(b) && !v.hotSet[b.Index] {
			calm = append(calm, b.Service.P99)
		}
	}
	v.baseline = median(calm)
	return v
}

// episode is one runner's merged hot run.
type episode struct {
	v *view
	span
}

// incidentWindow is a cluster of overlapping episodes from any runners.
type incidentWindow struct {
	span
	eps []episode
}

// cluster overlaps every runner's episodes, with a tolerance of tol buckets either
// side, into incident windows. Sorting by start and sweeping keeps it deterministic
// and linear.
func cluster(views []*view, p Params) []incidentWindow {
	var all []episode
	for _, v := range views {
		for _, s := range episodes(v.hot, p.MergeGap) {
			all = append(all, episode{v: v, span: s})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].v.runner.Name < all[j].v.runner.Name
	})
	var out []incidentWindow
	for _, e := range all {
		if n := len(out); n > 0 && e.start <= out[n-1].end+p.Tolerance {
			w := &out[n-1]
			w.eps = append(w.eps, e)
			if e.end > w.end {
				w.end = e.end
			}
			continue
		}
		out = append(out, incidentWindow{span: e.span, eps: []episode{e}})
	}
	return out
}

// incidentContext is everything classification needs beyond the windows.
type incidentContext struct {
	views    []*view
	bucketMS float64
	sloP99   float64 // the application's response-time p99 budget, 0 when unset
	signals  *signalIndex
	p        Params
}

func buildIncidents(ctx incidentContext) []result.Incident {
	windows := cluster(ctx.views, ctx.p)
	out := make([]result.Incident, 0, len(windows))
	for i, w := range windows {
		out = append(out, ctx.incident(i+1, w))
	}
	return out
}

func (ctx incidentContext) incident(n int, w incidentWindow) result.Incident {
	inc := result.Incident{
		ID:         fmt.Sprintf("inc-%d", n),
		StartIndex: w.start,
		EndIndex:   w.end,
		StartS:     round3(float64(w.start) * ctx.bucketMS / 1000),
		EndS:       round3(float64(w.end+1) * ctx.bucketMS / 1000),
	}
	inc.DurationS = round3(inc.EndS - inc.StartS)

	hotFirst := map[string]int{}
	for _, e := range w.eps {
		name := e.v.runner.Name
		if first, ok := hotFirst[name]; !ok || e.start < first {
			hotFirst[name] = e.start
		}
	}

	var (
		app          *view
		appHot       bool
		storageHot   []*view
		storageBlind bool
		storageCount int
		allDominated = true
	)
	for _, v := range ctx.views {
		ir := result.IncidentRunner{Name: v.runner.Name, BaselineP99: round3(v.baseline)}
		first, hot := hotFirst[v.runner.Name]
		ir.Hot = hot
		peak, sufficient := v.window(w.span)
		ir.PeakP99MS = round3(peak)
		if v.threshold > 0 {
			ir.Severity = round2(peak / v.threshold)
		}
		ir.Insufficient = !sufficient
		if hot {
			ir.FirstHotIdx = first
			if !v.clientDominated(w.span, ctx.p) {
				allDominated = false
			}
		}
		inc.Runners = append(inc.Runners, ir)

		switch {
		case v.app:
			if app == nil {
				app = v
			}
			appHot = appHot || hot
		default:
			storageCount++
			if hot {
				storageHot = append(storageHot, v)
			} else if !sufficient {
				storageBlind = true
			}
		}
	}

	inc.Telemetry = ctx.signals.within(w.span)

	switch {
	case allDominated:
		inc.Class = result.IncidentClientLimited
	case appHot && len(storageHot) > 0:
		inc.Class = result.IncidentCorrelated
	case !appHot:
		inc.Class = result.IncidentStorageOnly
	case storageCount == 0 || storageBlind:
		inc.Class = result.IncidentUnobserved
	default:
		inc.Class = result.IncidentAppOnly
	}

	if app != nil {
		inc.AppImpact = app.impact(w.span, ctx.sloP99)
	}
	if inc.Class == result.IncidentCorrelated {
		inc.Culprit = rankCulprit(hotFirst[app.runner.Name], storageHot, hotFirst, inc.Telemetry, w.span, ctx.p)
	}
	inc.Confidence = incidentConfidence(inc, storageHot)
	return inc
}

// window returns a runner's peak service p99 over a span and whether it had enough
// data there to be judged: at least half the span's buckets eligible.
func (v *view) window(s span) (peak float64, sufficient bool) {
	var ok int
	for i := s.start; i <= s.end; i++ {
		b, found := v.byIndex[i]
		if !found || !eligible(b) {
			continue
		}
		ok++
		if b.Service.P99 > peak {
			peak = b.Service.P99
		}
	}
	width := s.end - s.start + 1
	return peak, ok > 0 && 2*ok >= width
}

// clientDominated reports whether, in this runner's hot buckets inside the span, the
// generator's own waiting was the larger share of what a user would have felt.
func (v *view) clientDominated(s span, p Params) bool {
	var shares []float64
	for i := s.start; i <= s.end; i++ {
		if !v.hotSet[i] {
			continue
		}
		b := v.byIndex[i]
		if b == nil || b.Response.P99 <= 0 {
			continue
		}
		shares = append(shares, b.ClientWait.P99/b.Response.P99)
	}
	return len(shares) > 0 && median(shares) >= p.ClientDominance
}

func (v *view) impact(s span, sloP99 float64) *result.AppImpact {
	out := &result.AppImpact{}
	for i := s.start; i <= s.end; i++ {
		b, ok := v.byIndex[i]
		if !ok || b.Warmup {
			continue
		}
		out.AffectedOps += b.N
		if eligible(b) && b.Response.P99 > out.PeakP99MS {
			out.PeakP99MS = b.Response.P99
		}
	}
	out.PeakP99MS = round3(out.PeakP99MS)
	out.SLOBreached = sloP99 > 0 && out.PeakP99MS > sloP99
	return out
}

// rankCulprit orders the hot storage tiers of a correlated incident by lead time,
// then severity, then telemetry corroboration (ADR-005 §3), as a weighted score:
//
//	score = 2*lead + log2(severity) + corroborating signals
//
// with lead clamped to ±3 buckets, the severity term to [0, 4] and corroboration to
// two signals. Lead carries the most weight because cause precedes effect; severity
// is logarithmic so a tier ten times over its threshold does not drown out one that
// moved first. Scores within TieEpsilon of the best are a tie, and a tie is reported.
func rankCulprit(appFirst int, hot []*view, first map[string]int, signals []result.TelemetrySignal, w span, p Params) *result.Culprit {
	type scored struct {
		name    string
		score   float64
		lead    int
		reasons []string
	}
	var all []scored
	for _, v := range hot {
		name := v.runner.Name
		lead := appFirst - first[name]
		clamped := math.Max(-3, math.Min(3, float64(lead)))
		peak, _ := v.window(w)
		sev := 0.0
		if v.threshold > 0 {
			sev = peak / v.threshold
		}
		sevTerm := 0.0
		if sev > 1 {
			sevTerm = math.Min(4, math.Log2(sev))
		}
		var corr int
		var reasons []string
		switch {
		case lead > 0:
			reasons = append(reasons, fmt.Sprintf("went hot %d bucket(s) before the application", lead))
		case lead == 0:
			reasons = append(reasons, "went hot in the same bucket as the application")
		default:
			reasons = append(reasons, fmt.Sprintf("went hot %d bucket(s) after the application", -lead))
		}
		reasons = append(reasons, fmt.Sprintf("peak p99 %.1fx its threshold", round2(sev)))
		for _, s := range signals {
			if runnerForSource(s.Source) == name && corr < 2 {
				corr++
				reasons = append(reasons, "corroborated by "+s.Source+" "+s.Signal)
			}
		}
		all = append(all, scored{
			name: name, lead: lead, reasons: reasons,
			score: round2(2*clamped + sevTerm + float64(corr)),
		})
	}
	if len(all) == 0 {
		return nil
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].name < all[j].name
	})
	best := all[0]
	c := &result.Culprit{Runner: best.name, Score: best.score, LeadBuckets: best.lead, Reasons: best.reasons}
	for _, o := range all[1:] {
		if best.score-o.score < p.TieEpsilon {
			c.TiedWith = append(c.TiedWith, o.name)
		}
	}
	return c
}

// incidentConfidence says how much one incident is worth on its own.
func incidentConfidence(inc result.Incident, storageHot []*view) string {
	switch inc.Class {
	case result.IncidentCorrelated:
		switch {
		case inc.Culprit == nil || len(inc.Culprit.TiedWith) > 0:
			return result.ConfidenceLow
		case corroborates(inc.Telemetry, inc.Culprit.Runner):
			return result.ConfidenceHigh
		default:
			return result.ConfidenceMedium
		}
	case result.IncidentStorageOnly:
		for _, v := range storageHot {
			if corroborates(inc.Telemetry, v.runner.Name) {
				return result.ConfidenceHigh
			}
		}
		return result.ConfidenceMedium
	case result.IncidentAppOnly, result.IncidentClientLimited:
		return result.ConfidenceMedium
	default:
		return result.ConfidenceLow
	}
}

func corroborates(signals []result.TelemetrySignal, runner string) bool {
	for _, s := range signals {
		if runnerForSource(s.Source) == runner {
			return true
		}
	}
	return false
}
