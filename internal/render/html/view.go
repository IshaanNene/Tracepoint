package html

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/IshaanNene/Tracepoint/internal/render"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// view is the display data the report's script draws from: every series already
// aligned to one timeline and thinned for display. It is derived from the result and
// nothing else, and the script does no arithmetic of its own beyond placing points.
type view struct {
	RunID   string  `json:"run_id"`
	BucketS float64 `json:"bucket_s"`
	// Grouped is how many buckets each point stands for; 1 when nothing was thinned.
	Grouped   int             `json:"grouped"`
	X         []float64       `json:"x"`
	Runners   []viewRunner    `json:"runners"`
	Bands     []band          `json:"bands"`
	Telemetry []viewTelemetry `json:"telemetry,omitempty"`
	Capacity  *viewCapacity   `json:"capacity,omitempty"`
}

// viewRunner is one runner's timeline. A bucket with too few samples to support a
// percentile (§3.4) is left out of the line and drawn in its own points-only series
// instead: hiding it would mislead, and joining it to the line would lend it a
// weight it has not earned (ADR-002).
type viewRunner struct {
	Name     string                `json:"name"`
	Kind     string                `json:"kind"`
	Response map[string][]*float64 `json:"response"`
	Service  map[string][]*float64 `json:"service"`
	// Sparse holds the insufficient buckets, keyed like Response and Service with a
	// "response." or "service." prefix.
	Sparse   map[string][]*float64 `json:"sparse"`
	InFlight []*float64            `json:"in_flight"`
}

// band is a shaded span of the timeline.
type band struct {
	Kind  string  `json:"kind"`
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Label string  `json:"label"`
}

// Band kinds.
const (
	bandWarmup   = "warmup"
	bandRamp     = "ramp"
	bandIncident = "incident"
)

// viewTelemetry is one sampler's numeric series.
type viewTelemetry struct {
	Source  string                `json:"source"`
	Grouped int                   `json:"grouped"`
	X       []float64             `json:"x"`
	Metrics map[string][]*float64 `json:"metrics"`
}

// viewCapacity is the level table as series, ordered by the knob.
type viewCapacity struct {
	Knob        string     `json:"knob"`
	Value       []float64  `json:"value"`
	P99MS       []*float64 `json:"p99_ms"`
	AchievedRPS []*float64 `json:"achieved_rps"`
	OK          []bool     `json:"ok"`
}

var quantileNames = []string{"p50", "p95", "p99"}

func pick(q result.Quantiles, name string) float64 {
	switch name {
	case "p50":
		return q.P50
	case "p95":
		return q.P95
	default:
		return q.P99
	}
}

func buildView(r *result.Result) view {
	bucketS := r.Run.BucketMS / 1000
	v := view{RunID: r.Run.ID, BucketS: bucketS, Grouped: 1, Bands: []band{}}

	n := 0
	for _, rn := range r.Runners {
		for _, b := range rn.Buckets {
			n = max(n, b.Index+1)
		}
	}
	x := make([]float64, n)
	for i := range x {
		x[i] = float64(i) * bucketS
	}

	// Every series of every runner is thinned together, so points stay aligned.
	var all [][]*float64
	// kind: 0 response, 1 service, 2 in flight, 3 sparse response, 4 sparse service.
	type slot struct{ runner, kind, q int }
	var slots []slot
	for ri, rn := range r.Runners {
		byIndex := make([]*result.Bucket, n)
		for i := range rn.Buckets {
			if b := &rn.Buckets[i]; b.Index >= 0 && b.Index < n {
				byIndex[b.Index] = b
			}
		}
		for kind, get := range []func(*result.Bucket) result.Quantiles{
			func(b *result.Bucket) result.Quantiles { return b.Response },
			func(b *result.Bucket) result.Quantiles { return b.Service },
		} {
			for qi, q := range quantileNames {
				s := make([]*float64, n)
				sparse := make([]*float64, n)
				for i, b := range byIndex {
					if b == nil || b.N == 0 {
						continue
					}
					val := pick(get(b), q)
					if b.Insufficient {
						sparse[i] = &val
					} else {
						s[i] = &val
					}
				}
				all = append(all, s, sparse)
				slots = append(slots, slot{ri, kind, qi}, slot{ri, kind + 3, qi})
			}
		}
		inflight := make([]*float64, n)
		for i, b := range byIndex {
			if b != nil {
				val := float64(b.InFlightMax)
				inflight[i] = &val
			}
		}
		all = append(all, inflight)
		slots = append(slots, slot{ri, 2, 0})
	}
	gx, gys, group := downsample(x, all, maxPoints)
	v.X, v.Grouped = gx, group
	for _, rn := range r.Runners {
		v.Runners = append(v.Runners, viewRunner{
			Name: rn.Name, Kind: rn.Kind,
			Response: map[string][]*float64{}, Service: map[string][]*float64{}, Sparse: map[string][]*float64{},
		})
	}
	for i, s := range slots {
		vr := &v.Runners[s.runner]
		switch s.kind {
		case 0:
			vr.Response[quantileNames[s.q]] = gys[i]
		case 1:
			vr.Service[quantileNames[s.q]] = gys[i]
		case 3:
			vr.Sparse["response."+quantileNames[s.q]] = gys[i]
		case 4:
			vr.Sparse["service."+quantileNames[s.q]] = gys[i]
		default:
			vr.InFlight = gys[i]
		}
	}

	v.Bands = bands(r, bucketS)
	v.Telemetry = telemetryViews(r)
	v.Capacity = capacityView(r)
	return v
}

// bands shades warm-up, every ramping stage and every incident.
func bands(r *result.Result, bucketS float64) []band {
	out := []band{}
	if r.Run.WarmupMS > 0 {
		out = append(out, band{Kind: bandWarmup, From: 0, To: r.Run.WarmupMS / 1000, Label: "warm-up (excluded from summaries)"})
	}
	seen := map[[2]float64]bool{}
	for _, rn := range r.Runners {
		ex := rn.Executor
		if ex == nil || ex.StartTarget == nil {
			continue // a 1.0 result cannot tell a ramp from a hold
		}
		prev, at := *ex.StartTarget, 0.0
		for _, st := range ex.Stages {
			end := at + st.DurationMS/1000
			if st.Target != prev && !seen[[2]float64{at, end}] {
				seen[[2]float64{at, end}] = true
				out = append(out, band{Kind: bandRamp, From: at, To: end,
					Label: fmt.Sprintf("%s ramps %s to %s", rn.Name, render.Number(prev), render.Number(st.Target))})
			}
			prev, at = st.Target, end
		}
	}
	for _, inc := range r.Analysis.Incidents {
		label := inc.ID + " " + inc.Class
		if inc.Culprit != nil {
			label += ", culprit " + inc.Culprit.Runner
		}
		out = append(out, band{Kind: bandIncident, From: float64(inc.StartIndex) * bucketS,
			To: float64(inc.EndIndex+1) * bucketS, Label: label})
	}
	return out
}

func telemetryViews(r *result.Result) []viewTelemetry {
	t := r.Telemetry
	if t == nil {
		return nil
	}
	var out []viewTelemetry
	add := func(source string, samples []map[string]any) {
		if vt, ok := telemetryView(source, samples); ok {
			out = append(out, vt)
		}
	}
	if t.Generator != nil {
		add("generator", t.Generator.Samples)
	}
	for _, s := range []struct {
		name   string
		series *result.SamplerSeries
	}{{"postgres", t.Postgres}, {"mysql", t.MySQL}, {"redis", t.Redis}} {
		if s.series != nil && s.series.Available {
			add(s.name, s.series.Samples)
		}
	}
	return out
}

// telemetryView turns samples into aligned numeric series. Non-numeric fields are
// left to the tables; a metric absent from a sample is a gap.
func telemetryView(source string, samples []map[string]any) (viewTelemetry, bool) {
	keys := map[string]bool{}
	var x []float64
	var rows []map[string]any
	for _, s := range samples {
		t, ok := number(s["t_ms"])
		if !ok {
			continue
		}
		x = append(x, t/1000)
		rows = append(rows, s)
		for k, val := range s {
			if _, isNum := number(val); isNum && k != "t_ms" {
				keys[k] = true
			}
		}
	}
	if len(x) == 0 || len(keys) == 0 {
		return viewTelemetry{}, false
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	series := make([][]*float64, len(names))
	for i, name := range names {
		series[i] = make([]*float64, len(rows))
		for j, row := range rows {
			if val, ok := number(row[name]); ok {
				series[i][j] = &val
			}
		}
	}
	gx, gys, group := downsample(x, series, maxPoints)
	vt := viewTelemetry{Source: source, Grouped: group, X: gx, Metrics: map[string][]*float64{}}
	for i, name := range names {
		vt.Metrics[name] = gys[i]
	}
	return vt, true
}

// number reads a sample value, which is float64 after a round trip through JSON but
// may be any numeric type in a result built in memory.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func capacityView(r *result.Result) *viewCapacity {
	c := r.Capacity
	if c == nil || len(c.Levels) == 0 {
		return nil
	}
	levels := append([]result.CapacityLevel(nil), c.Levels...)
	sort.SliceStable(levels, func(i, j int) bool { return levels[i].Value < levels[j].Value })
	out := &viewCapacity{Knob: c.Knob}
	for _, l := range levels {
		p99, rps := l.P99MS, l.AchievedRPS
		out.Value = append(out.Value, l.Value)
		out.P99MS = append(out.P99MS, &p99)
		out.AchievedRPS = append(out.AchievedRPS, &rps)
		out.OK = append(out.OK, l.OK)
	}
	return out
}
