package capacity

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"go.uber.org/goleak"
	"pgregory.net/rapid"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// usl is the model itself, for building exact data.
func usl(lambda, sigma, kappa, n float64) float64 {
	return lambda * n / (1 + sigma*(n-1) + kappa*n*(n-1))
}

func exact(lambda, sigma, kappa float64, ns ...float64) []Point {
	out := make([]Point, 0, len(ns))
	for _, n := range ns {
		out = append(out, Point{N: n, X: usl(lambda, sigma, kappa, n)})
	}
	return out
}

func near(got, want, rel float64) bool {
	if want == 0 {
		return math.Abs(got) <= rel
	}
	return math.Abs(got-want) <= rel*math.Abs(want)
}

func TestUSLRecoversKnownParameters(t *testing.T) {
	fit := FitUSL(exact(100, 0.05, 0.001, 1, 2, 4, 8, 16, 32, 64))
	if !fit.Fitted {
		t.Fatalf("not fitted: %s", fit.Reason)
	}
	if !near(fit.Lambda, 100, 1e-3) || !near(fit.Sigma, 0.05, 1e-2) || !near(fit.Kappa, 0.001, 1e-2) {
		t.Fatalf("fit = %+v", fit)
	}
	if fit.R2 < 0.9999 {
		t.Fatalf("R2 = %v on exact data", fit.R2)
	}
	wantPeak := math.Sqrt(0.95 / 0.001)
	if !near(fit.PeakN, wantPeak, 1e-2) || !near(fit.PeakThroughput, usl(100, 0.05, 0.001, wantPeak), 1e-2) {
		t.Fatalf("peak %v at %v, want %v at %v", fit.PeakThroughput, fit.PeakN, usl(100, 0.05, 0.001, wantPeak), wantPeak)
	}
}

// Perfectly linear scaling has no peak to predict: kappa is zero, and a peak is not
// invented.
func TestUSLLinearScalingHasNoPeak(t *testing.T) {
	fit := FitUSL(exact(40, 0, 0, 1, 2, 3, 4, 5))
	if !fit.Fitted || fit.Sigma > 1e-9 || fit.Kappa > 1e-9 {
		t.Fatalf("fit = %+v", fit)
	}
	if fit.PeakN != 0 || fit.PeakThroughput != 0 {
		t.Fatalf("a peak was predicted for linear scaling: %+v", fit)
	}
}

// Superlinear data would want a negative sigma; the fit is constrained to the
// physically meaningful region.
func TestUSLCoefficientsAreNeverNegative(t *testing.T) {
	pts := []Point{{1, 10}, {2, 22}, {4, 48}, {8, 100}, {16, 210}}
	fit := FitUSL(pts)
	if fit.Sigma < 0 || fit.Kappa < 0 {
		t.Fatalf("negative coefficient: %+v", fit)
	}
}

func TestUSLNeedsFourLevels(t *testing.T) {
	fit := FitUSL(exact(100, 0.05, 0.001, 1, 2, 4))
	if fit.Fitted || !strings.Contains(fit.Reason, "4") {
		t.Fatalf("fit = %+v", fit)
	}
}

// A fit that explains too little is hidden, and says why.
func TestUSLHiddenBelowR2(t *testing.T) {
	pts := []Point{{1, 100}, {2, 20}, {3, 180}, {4, 30}, {5, 150}, {6, 10}}
	fit := FitUSL(pts)
	if fit.Fitted || !strings.Contains(fit.Reason, "R²") {
		t.Fatalf("fit = %+v", fit)
	}
	if fit.R2 >= MinR2 {
		t.Fatalf("R2 = %v", fit.R2)
	}
}

func TestUSLIgnoresUnusablePoints(t *testing.T) {
	pts := append(exact(100, 0.05, 0.001, 1, 2, 4, 8), Point{N: 0, X: 5}, Point{N: 3, X: 0}, Point{N: math.NaN(), X: 1})
	fit := FitUSL(pts)
	if !fit.Fitted || !near(fit.Sigma, 0.05, 1e-2) {
		t.Fatalf("fit = %+v", fit)
	}
}

// Property: on noise-free data from any plausible system, the fitted curve reproduces
// every observation.
func TestUSLReproducesExactDataProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		lambda := rapid.Float64Range(1, 5000).Draw(rt, "lambda")
		sigma := rapid.Float64Range(0, 0.5).Draw(rt, "sigma")
		kappa := rapid.Float64Range(0, 0.01).Draw(rt, "kappa")
		pts := exact(lambda, sigma, kappa, 1, 2, 4, 8, 16, 32)
		fit := FitUSL(pts)
		if fit.Lambda == 0 {
			rt.Fatalf("no fit: %+v", fit)
		}
		for _, p := range pts {
			if got := usl(fit.Lambda, fit.Sigma, fit.Kappa, p.N); !near(got, p.X, 0.01) {
				rt.Fatalf("X(%v) = %v, observed %v (fit %+v)", p.N, got, p.X, fit)
			}
		}
	})
}

// With modest noise the fit stays close and is still reported.
func TestUSLToleratesNoise(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	pts := exact(200, 0.08, 0.0005, 1, 2, 4, 8, 16, 32, 64)
	for i := range pts {
		pts[i].X *= 1 + (rng.Float64()-0.5)*0.04
	}
	fit := FitUSL(pts)
	if !fit.Fitted || !near(fit.Lambda, 200, 0.05) || fit.R2 < 0.98 {
		t.Fatalf("fit = %+v", fit)
	}
}
