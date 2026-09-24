package analysis

import (
	"math"
	"strconv"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Correlation leans.
const (
	LeanStorage = "storage"
	LeanApp     = "app"
	LeanNone    = "none"
)

// correlate computes Spearman's rho between the application's service-time p99 series
// and one storage runner's, at every lag from -MaxLag to +MaxLag.
//
// At lag L the application's bucket i is paired with the storage runner's bucket i-L,
// so a positive lag means the storage tier moved first - what one expects when it is
// the cause. Only pairs where both buckets are eligible are used. The best lag is the
// one with the largest |rho|; ties go to the smaller |lag|, then to the positive side,
// so the choice never depends on iteration order.
func correlate(app, storage *view, p Params) result.Correlation {
	c := result.Correlation{Storage: storage.runner.Name, RhoByLag: map[string]float64{}, Lean: LeanNone}
	bestAbs := -1.0
	for _, lag := range lagOrder(p.MaxLag) {
		var xs, ys []float64
		for i := range app.runner.Buckets {
			a := &app.runner.Buckets[i]
			if !eligible(a) {
				continue
			}
			s, ok := storage.byIndex[a.Index-lag]
			if !ok || !eligible(s) {
				continue
			}
			xs = append(xs, a.Service.P99)
			ys = append(ys, s.Service.P99)
		}
		rho := 0.0
		if len(xs) >= 3 {
			rho = round3(spearman(xs, ys))
		}
		c.RhoByLag[strconv.Itoa(lag)] = rho
		if math.Abs(rho) > bestAbs {
			bestAbs = math.Abs(rho)
			c.Rho, c.BestLag, c.NBuckets = rho, lag, len(xs)
		}
	}
	switch {
	case c.NBuckets < p.MinPairs || c.Rho < p.LeanRho:
		c.Lean = LeanNone
	case c.BestLag < 0:
		// The application moved first and storage followed: consistent with the
		// application's own load driving the storage tier, not the reverse.
		c.Lean = LeanApp
	default:
		c.Lean = LeanStorage
	}
	return c
}

// lagOrder lists lags 0, +1, -1, +2, -2, ... so that the first maximum found is the
// preferred one on a tie.
func lagOrder(maxLag int) []int {
	out := []int{0}
	for l := 1; l <= maxLag; l++ {
		out = append(out, l, -l)
	}
	return out
}
