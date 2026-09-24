// Package resulttest builds synthetic result documents for tests: a clean three-tier
// run on one clock, into which faults can be placed at known buckets.
package resulttest

import (
	"github.com/IshaanNene/Tracepoint/internal/analysis"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Run builds a three-tier result with n one-second buckets. http sits at 20ms, db at
// 2ms and redis at 0.5ms; faults raise a runner's p99 over a range of buckets.
type Run struct {
	R *result.Result
}

// NewRun starts a clean run of n one-second buckets.
func NewRun(n int) *Run {
	r := &result.Result{
		SchemaVersion: result.SchemaVersion,
		Tool:          result.Tool{Name: "tracepoint", Version: "test"},
		Run: result.Run{
			ID: "20260924T100000Z-abc123", Name: "checkout", Status: result.StatusCompleted,
			StartedAt: "2026-09-24T10:00:00Z", DurationMS: float64(n) * 1000, ElapsedMS: float64(n) * 1000,
			BucketMS: 1000, BucketCount: n, MinSamples: 20, Seed: 7,
		},
		Config: &result.Config{Hash: "sha256:test", Effective: map[string]any{
			"http":  map[string]any{"executor": map[string]any{"rate": 100.0, "max_in_flight": 4.0}},
			"db":    map[string]any{"driver": "postgres", "executor": map[string]any{"rate": 10.0}},
			"redis": map[string]any{"executor": map[string]any{"rate": 50.0}},
		}},
	}
	for _, rn := range []struct {
		name, kind string
		base       float64
	}{{"http", "app", 20}, {"db", "storage", 2}, {"redis", "storage", 0.5}} {
		wobble := []float64{0, 0.03, -0.02, 0.05, -0.04, 0.01}
		var buckets []result.Bucket
		for i := range n {
			v := rn.base * (1 + wobble[i%len(wobble)])
			buckets = append(buckets, result.Bucket{
				Index: i, OffsetMS: float64(i) * 1000, N: 100, Offered: 100,
				Service:    result.Quantiles{P50: v / 2, P99: v, Mean: v / 2},
				Response:   result.Quantiles{P50: v / 2, P99: v + 0.1},
				ClientWait: result.Quantiles{P99: 0.1},
			})
		}
		out := result.Runner{
			Name: rn.name, Kind: rn.kind, Buckets: buckets,
			Summary: result.OpSummary{
				N: int64(100 * n), OK: int64(100 * n), AchievedRPS: 100,
				Response: result.Quantiles{P50: rn.base / 2, P95: rn.base * 0.9, P99: rn.base},
				Service:  result.Quantiles{P99: rn.base},
			},
			Executor: &result.ExecutorStats{Type: "arrival-rate", Offered: int64(100 * n), Dispatched: int64(100 * n), MaxInFlight: 4},
		}
		if rn.name == "db" {
			out.Driver = "postgres"
		}
		r.Runners = append(r.Runners, out)
	}
	return &Run{R: r}
}

// Fault raises a runner's p99 service and response time over buckets from..to.
func (b *Run) Fault(runner string, from, to int, p99 float64) *Run {
	rn := b.R.RunnerByName(runner)
	for i := from; i <= to; i++ {
		rn.Buckets[i].Service.P99 = p99
		rn.Buckets[i].Response.P99 = p99 + 0.1
	}
	return b
}

// Analysed runs the analysis over the run and returns the finished result.
func (b *Run) Analysed(in analysis.Inputs) *result.Result {
	b.R.Analysis = analysis.Analyse(b.R, in)
	return b.R
}
