package result

import "math"

// OfferedRPS is the rate the schedule asked this runner for over the measured window,
// which is what a pool has to be sized for - not the lower rate it managed.
func (r *Runner) OfferedRPS(bucketMS float64) float64 {
	var offered int64
	var buckets int
	for i := range r.Buckets {
		b := &r.Buckets[i]
		if b.Warmup {
			continue
		}
		offered += b.Offered
		buckets++
	}
	if buckets == 0 || offered == 0 || bucketMS <= 0 {
		return r.Summary.AchievedRPS
	}
	return float64(offered) / (float64(buckets) * bucketMS / 1000)
}

// NeededInFlight is the concurrency the offered rate needed, by Little's Law: in
// flight = rate x service time, taken at p99 so the tail does not starve the pool,
// with 20% headroom.
//
// It is asked for only when the configured concurrency proved too few, so it never
// answers with less than twice what was configured: a figure at or below the setting
// that just failed would be advice to change nothing.
func (r *Runner) NeededInFlight(bucketMS float64) int {
	if r.Executor == nil {
		return 0
	}
	needed := int(math.Ceil(r.OfferedRPS(bucketMS) * r.Summary.Service.P99 / 1000 * 1.2))
	if needed <= r.Executor.MaxInFlight {
		needed = r.Executor.MaxInFlight * 2
	}
	return needed
}
