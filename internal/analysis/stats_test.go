package analysis

import (
	"math"
	"sort"
	"testing"

	"pgregory.net/rapid"
)

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{7}, 7},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{5, 5, 5, 100}, 5},
	}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// median must not reorder the caller's slice: the hot-bucket rule calls it on a window
// that is still in timeline order.
func TestMedianDoesNotMutate(t *testing.T) {
	in := []float64{3, 1, 2}
	median(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Fatalf("median reordered its input: %v", in)
	}
}

func TestMAD(t *testing.T) {
	// median 3, deviations 2,1,0,1,97 -> median deviation 1.
	if got := mad([]float64{1, 2, 3, 4, 100}, 3); got != 1 {
		t.Fatalf("mad = %v, want 1", got)
	}
}

// A textbook example: ranks with ties are averaged.
func TestRanksAverageTies(t *testing.T) {
	got := ranks([]float64{10, 20, 20, 30})
	want := []float64{1, 2.5, 2.5, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranks = %v, want %v", got, want)
		}
	}
}

func TestSpearman(t *testing.T) {
	cases := []struct {
		name string
		x, y []float64
		want float64
	}{
		{"identical", []float64{1, 2, 3, 4, 5}, []float64{1, 2, 3, 4, 5}, 1},
		{"reversed", []float64{1, 2, 3, 4, 5}, []float64{5, 4, 3, 2, 1}, -1},
		// Monotonic but wildly non-linear: Pearson would not say 1, Spearman must.
		{"monotonic convex", []float64{1, 2, 3, 4, 5}, []float64{1, 10, 100, 1000, 10000}, 1},
		// A classic worked example (Spearman, textbook): rho = 1 - 6*sum(d^2)/(n(n^2-1)).
		{"worked", []float64{106, 86, 100, 101, 99, 103, 97, 113, 112, 110},
			[]float64{7, 0, 27, 50, 28, 29, 20, 12, 6, 17}, -0.175757575},
		{"constant", []float64{1, 1, 1, 1}, []float64{1, 2, 3, 4}, 0},
		{"too short", []float64{1}, []float64{1}, 0},
	}
	for _, c := range cases {
		if got := spearman(c.x, c.y); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: spearman = %v, want %v", c.name, got, c.want)
		}
	}
}

// Spearman is bounded, symmetric, and invariant under any strictly increasing
// transformation - which is the whole reason it was chosen over Pearson (ADR-005).
func TestSpearmanProperties(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(3, 60).Draw(rt, "n")
		x := rapid.SliceOfN(rapid.Float64Range(0.1, 1e4), n, n).Draw(rt, "x")
		y := rapid.SliceOfN(rapid.Float64Range(0.1, 1e4), n, n).Draw(rt, "y")

		rho := spearman(x, y)
		if rho < -1-1e-9 || rho > 1+1e-9 || math.IsNaN(rho) {
			rt.Fatalf("rho out of range: %v", rho)
		}
		if back := spearman(y, x); math.Abs(back-rho) > 1e-9 {
			rt.Fatalf("not symmetric: %v vs %v", rho, back)
		}
		logged := make([]float64, n)
		for i, v := range y {
			logged[i] = math.Log(v) * 3
		}
		if tr := spearman(x, logged); math.Abs(tr-rho) > 1e-9 {
			rt.Fatalf("not invariant under a monotonic transform: %v vs %v", rho, tr)
		}
	})
}

func TestMedianProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		v := rapid.SliceOfN(rapid.Float64Range(-1e6, 1e6), 1, 80).Draw(rt, "v")
		m := median(v)
		sorted := append([]float64(nil), v...)
		sort.Float64s(sorted)
		var below, above int
		for _, x := range sorted {
			if x < m {
				below++
			}
			if x > m {
				above++
			}
		}
		if below > len(v)/2 || above > len(v)/2 {
			rt.Fatalf("median %v splits %v unevenly: %d below, %d above", m, sorted, below, above)
		}
	})
}
