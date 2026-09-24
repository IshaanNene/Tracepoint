package telemetry

import (
	"context"
	"runtime"
	"runtime/metrics"
	"time"
)

// RunnerHealth is what the engine reports about one runner at a sample.
type RunnerHealth struct {
	InFlight int64
	// DispatchLagP99MS is the p99 lag since the previous sample; HasLag is false
	// when nothing was dispatched in between.
	DispatchLagP99MS float64
	HasLag           bool
}

// Generator samples the load generator's own health: the evidence for "we could not
// ask fast enough" as opposed to "the target was slow".
//
// Keys:
//
//	cpu_ratio             process CPU over the interval / (wall time x GOMAXPROCS)
//	goroutines, heap_bytes
//	gc_pause_p99_ms       p99 stop-the-world pause over the interval
//	sched_latency_p99_ms  p99 time a runnable goroutine waited to run, over the interval
//	dispatch_lag_p99_ms   per runner, how late arrivals were handed out
//	in_flight             per runner, operations in progress
type Generator struct {
	now     func() time.Time
	runners func() map[string]RunnerHealth

	samples   []metrics.Sample
	prevCPU   time.Duration
	prevWall  time.Time
	primed    bool
	prevPause *metrics.Float64Histogram
	prevSched *metrics.Float64Histogram
}

const (
	metricGoroutines = "/sched/goroutines:goroutines"
	metricHeap       = "/memory/classes/heap/objects:bytes"
	metricPauses     = "/sched/pauses/total/gc:seconds"
	metricLatencies  = "/sched/latencies:seconds"
)

// NewGenerator builds the generator sampler. runners is called at every sample for
// the per-runner view and may be nil.
func NewGenerator(now func() time.Time, runners func() map[string]RunnerHealth) *Generator {
	if now == nil {
		now = time.Now
	}
	g := &Generator{now: now, runners: runners}
	for _, name := range []string{metricGoroutines, metricHeap, metricPauses, metricLatencies} {
		g.samples = append(g.samples, metrics.Sample{Name: name})
	}
	return g
}

// Name identifies the sampler.
func (g *Generator) Name() string { return "generator" }

// Sample reads the runtime's counters.
func (g *Generator) Sample(_ context.Context, _ time.Duration) (Sample, error) {
	metrics.Read(g.samples)
	s := Sample{}
	for _, m := range g.samples {
		switch m.Name {
		case metricGoroutines:
			if m.Value.Kind() == metrics.KindUint64 {
				s["goroutines"] = int64(m.Value.Uint64()) //nolint:gosec // a goroutine count fits
			}
		case metricHeap:
			if m.Value.Kind() == metrics.KindUint64 {
				s["heap_bytes"] = int64(m.Value.Uint64()) //nolint:gosec // a heap size fits
			}
		case metricPauses:
			if m.Value.Kind() == metrics.KindFloat64Histogram {
				h := m.Value.Float64Histogram()
				if g.prevPause != nil {
					if v, ok := histP99(g.prevPause, h); ok {
						s["gc_pause_p99_ms"] = round3(v * 1000)
					}
				}
				g.prevPause = copyHist(h)
			}
		case metricLatencies:
			if m.Value.Kind() == metrics.KindFloat64Histogram {
				h := m.Value.Float64Histogram()
				if g.prevSched != nil {
					if v, ok := histP99(g.prevSched, h); ok {
						s["sched_latency_p99_ms"] = round3(v * 1000)
					}
				}
				g.prevSched = copyHist(h)
			}
		}
	}

	now := g.now()
	if cpu, ok := processCPU(); ok {
		if g.primed {
			wall := now.Sub(g.prevWall)
			if wall > 0 {
				s["cpu_ratio"] = round3(float64(cpu-g.prevCPU) / (float64(wall) * float64(runtime.GOMAXPROCS(0))))
			}
		}
		g.prevCPU, g.prevWall, g.primed = cpu, now, true
	}

	if g.runners != nil {
		lag := map[string]any{}
		inFlight := map[string]any{}
		for name, h := range g.runners() {
			inFlight[name] = h.InFlight
			if h.HasLag {
				lag[name] = round3(h.DispatchLagP99MS)
			}
		}
		if len(inFlight) > 0 {
			s["in_flight"] = inFlight
		}
		if len(lag) > 0 {
			s["dispatch_lag_p99_ms"] = lag
		}
	}
	return s, nil
}

// histP99 returns the p99 of the observations added between two readings of a
// cumulative runtime histogram. The value is the upper bound of the bucket the
// quantile falls in, or its lower bound when that bucket is unbounded above - the
// conservative reading either way.
func histP99(prev, cur *metrics.Float64Histogram) (float64, bool) {
	if len(prev.Counts) != len(cur.Counts) {
		return 0, false
	}
	var total uint64
	delta := make([]uint64, len(cur.Counts))
	for i := range cur.Counts {
		if cur.Counts[i] >= prev.Counts[i] {
			delta[i] = cur.Counts[i] - prev.Counts[i]
		}
		total += delta[i]
	}
	if total == 0 {
		return 0, false
	}
	rank := uint64(float64(total) * 0.99)
	if rank == 0 {
		rank = 1
	}
	var seen uint64
	for i, n := range delta {
		seen += n
		if seen >= rank {
			hi := cur.Buckets[i+1]
			if hi > 1e300 { // +Inf
				return cur.Buckets[i], true
			}
			return hi, true
		}
	}
	return 0, false
}

func copyHist(h *metrics.Float64Histogram) *metrics.Float64Histogram {
	return &metrics.Float64Histogram{
		Counts:  append([]uint64(nil), h.Counts...),
		Buckets: h.Buckets, // immutable for the life of the process
	}
}
