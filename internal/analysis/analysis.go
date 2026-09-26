// Package analysis contains the pure functions that turn recorded data into findings:
// hot buckets, incidents, culprit ranking, lagged correlation, verdicts, run validity
// and SLO evaluation. See docs/adr/005-correlation-methodology.md and
// docs/METHODOLOGY.md.
//
// Everything here is a pure function of a result document and a few inputs taken from
// the configuration. Nothing reads a clock, a random source or the network, so the
// same document always produces the same analysis in the same words - which is what
// makes a verdict auditable and golden-file testable.
package analysis

import (
	"sort"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Analyse derives everything that can be concluded from a recorded run.
func Analyse(r *result.Result, in Inputs) result.Analysis {
	p := in.params()

	names := make([]string, 0, len(r.Runners))
	for i := range r.Runners {
		names = append(names, r.Runners[i].Name)
	}

	a := result.Analysis{
		Validity:   evaluateValidity(r, in),
		SLO:        evaluateSLO(r, in),
		Thresholds: thresholds(names, in),
	}

	stalled := map[int]bool{}
	for _, i := range generatorStalls(r.Runners, a.Thresholds, p) {
		stalled[i] = true
	}

	views := make([]*view, 0, len(r.Runners))
	for i := range r.Runners {
		rn := &r.Runners[i]
		th := a.Thresholds[rn.Name].ThresholdMS
		hot := hotBucketsExcept(rn.Buckets, th, p, stalled)
		if len(hot) > 0 {
			if a.HotBuckets == nil {
				a.HotBuckets = map[string][]result.HotBucket{}
			}
			a.HotBuckets[rn.Name] = hot
		}
		views = append(views, newView(rn, th, hot))
	}

	var app *view
	for _, v := range views {
		if v.app {
			app = v
			break
		}
	}

	sloP99 := 0.0
	if app != nil {
		if t := in.SLO.For(app.runner.Name); t.P99 != nil {
			sloP99 = msOf(t.P99.D())
		}
	}
	a.Incidents = buildIncidents(incidentContext{
		views:    views,
		bucketMS: r.Run.BucketMS,
		sloP99:   sloP99,
		signals:  newSignalIndex(r.Telemetry, r.Run.BucketMS, r.Run.WarmupMS),
		p:        p,
	})

	if app != nil {
		for _, v := range views {
			if !v.app {
				a.Correlation = append(a.Correlation, correlate(app, v, p))
			}
		}
		sort.SliceStable(a.Correlation, func(i, j int) bool { return a.Correlation[i].Storage < a.Correlation[j].Storage })
	}

	a.Verdict = buildVerdict(r, a, views, p)
	return a
}
