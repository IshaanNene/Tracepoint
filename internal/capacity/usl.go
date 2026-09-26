package capacity

import (
	"fmt"
	"math"
)

// MinUSLPoints and MinR2 decide whether a Universal Scalability Law fit is reported
// (spec §5.6). An extrapolation from too few levels, or one that explains too little
// of what was measured, is worse than none.
const (
	MinUSLPoints = 4
	MinR2        = 0.9
)

// Fit is a Universal Scalability Law fit. When Fitted is false, Reason says why and
// only R2 may be set.
type Fit struct {
	Fitted         bool
	Reason         string
	Lambda         float64
	Sigma          float64
	Kappa          float64
	R2             float64
	PeakN          float64
	PeakThroughput float64
}

// Point is one level that held: measured mean concurrency and throughput.
type Point struct {
	N float64
	X float64
}

// FitUSL fits X(N) = λN / (1 + σ(N−1) + κN(N−1)) by constrained least squares on
// throughput, with σ, κ ≥ 0.
//
// For a fixed λ the model is linear in σ and κ after rearranging - λN/X − 1 =
// σ(N−1) + κN(N−1) - so the inner problem is a two-variable non-negative least
// squares, solved exactly. The outer problem is a one-dimensional search over λ,
// minimising the squared error in throughput itself: a log-spaced scan to find the
// basin, then golden-section refinement inside it.
func FitUSL(points []Point) Fit {
	var pts []Point
	for _, p := range points {
		if p.N > 0 && p.X > 0 && !math.IsNaN(p.N) && !math.IsNaN(p.X) && !math.IsInf(p.N, 0) && !math.IsInf(p.X, 0) {
			pts = append(pts, p)
		}
	}
	if len(pts) < MinUSLPoints {
		return Fit{Reason: fmt.Sprintf("a fit needs at least %d levels that held; there were %d", MinUSLPoints, len(pts))}
	}

	perUnit := 0.0
	for _, p := range pts {
		perUnit = math.Max(perUnit, p.X/p.N)
	}
	// The denominator is at least 1, so λ ≥ X/N at every point, give or take noise;
	// how far above depends on how much contention the lowest level already carried.
	lo, hi := math.Log(perUnit*0.5), math.Log(perUnit*1e4)
	const scan = 600
	bestI, bestE := 0, math.Inf(1)
	for i := 0; i <= scan; i++ {
		l := math.Exp(lo + (hi-lo)*float64(i)/scan)
		if _, _, e := inner(pts, l); e < bestE {
			bestI, bestE = i, e
		}
	}
	a := lo + (hi-lo)*float64(max(bestI-1, 0))/scan
	b := lo + (hi-lo)*float64(min(bestI+1, scan))/scan
	lambda := goldenMin(a, b, func(x float64) float64 {
		_, _, e := inner(pts, math.Exp(x))
		return e
	})
	lambda = math.Exp(lambda)
	sigma, kappa, sse := inner(pts, lambda)

	mean := 0.0
	for _, p := range pts {
		mean += p.X
	}
	mean /= float64(len(pts))
	sst := 0.0
	for _, p := range pts {
		sst += (p.X - mean) * (p.X - mean)
	}
	r2 := 1.0
	if sst > 0 {
		r2 = 1 - sse/sst
	}
	if r2 < MinR2 {
		return Fit{R2: r2, Reason: fmt.Sprintf("the fit explains too little of what was measured (R² %.2f, below %.2f)", r2, MinR2)}
	}

	out := Fit{Fitted: true, Lambda: lambda, Sigma: sigma, Kappa: kappa, R2: r2}
	if kappa > 0 && sigma < 1 {
		out.PeakN = math.Sqrt((1 - sigma) / kappa)
		out.PeakThroughput = model(lambda, sigma, kappa, out.PeakN)
	}
	return out
}

func model(lambda, sigma, kappa, n float64) float64 {
	return lambda * n / (1 + sigma*(n-1) + kappa*n*(n-1))
}

// inner solves for σ, κ ≥ 0 at a fixed λ and returns the squared throughput error.
func inner(pts []Point, lambda float64) (sigma, kappa, sse float64) {
	var saa, sab, sbb, say, sby float64
	for _, p := range pts {
		y := lambda*p.N/p.X - 1
		a, b := p.N-1, p.N*(p.N-1)
		saa += a * a
		sab += a * b
		sbb += b * b
		say += a * y
		sby += b * y
	}
	// Candidates: the unconstrained optimum when feasible, then each face of the
	// feasible quadrant. The best of these by the linearised residual is the
	// non-negative optimum.
	type cand struct{ s, k float64 }
	cands := []cand{{0, 0}}
	if det := saa*sbb - sab*sab; det > 0 {
		s, k := (say*sbb-sby*sab)/det, (sby*saa-say*sab)/det
		if s >= 0 && k >= 0 {
			cands = append(cands, cand{s, k})
		}
	}
	if saa > 0 {
		cands = append(cands, cand{math.Max(0, say/saa), 0})
	}
	if sbb > 0 {
		cands = append(cands, cand{0, math.Max(0, sby/sbb)})
	}
	best, bestR := cands[0], math.Inf(1)
	for _, c := range cands {
		r := 0.0
		for _, p := range pts {
			d := lambda*p.N/p.X - 1 - c.s*(p.N-1) - c.k*p.N*(p.N-1)
			r += d * d
		}
		if r < bestR {
			best, bestR = c, r
		}
	}
	for _, p := range pts {
		d := model(lambda, best.s, best.k, p.N) - p.X
		sse += d * d
	}
	return best.s, best.k, sse
}

// goldenMin minimises f on [a, b], assumed unimodal there.
func goldenMin(a, b float64, f func(float64) float64) float64 {
	const phi = 0.6180339887498949
	c, d := b-phi*(b-a), a+phi*(b-a)
	fc, fd := f(c), f(d)
	for range 200 {
		if math.Abs(b-a) < 1e-12 {
			break
		}
		if fc < fd {
			b, d, fd = d, c, fc
			c = b - phi*(b-a)
			fc = f(c)
		} else {
			a, c, fc = c, d, fd
			d = a + phi*(b-a)
			fd = f(d)
		}
	}
	return (a + b) / 2
}
