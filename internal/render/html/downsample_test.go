package html

import (
	"math"
	"testing"

	"pgregory.net/rapid"
)

func f(v float64) *float64 { return &v }

func TestDownsampleLeavesShortSeriesAlone(t *testing.T) {
	x := []float64{0, 1, 2}
	ys := [][]*float64{{f(1), nil, f(3)}}
	gx, gys, group := downsample(x, ys, 5)
	if group != 1 || len(gx) != 3 || gys[0][1] != nil || *gys[0][2] != 3 {
		t.Fatalf("got x=%v group=%d", gx, group)
	}
}

// Groups take their first x and the maximum of each series, so a one-bucket spike
// survives any amount of thinning; a group with no data stays a gap.
func TestDownsampleKeepsMaximaAndGaps(t *testing.T) {
	x := []float64{0, 1, 2, 3, 4, 5, 6}
	ys := [][]*float64{{f(1), f(9), f(2), nil, nil, f(4), f(5)}}
	gx, gys, group := downsample(x, ys, 3)
	if group != 3 {
		t.Fatalf("group = %d, want 3", group)
	}
	if len(gx) != 3 || gx[0] != 0 || gx[1] != 3 || gx[2] != 6 {
		t.Fatalf("x = %v", gx)
	}
	want := []*float64{f(9), f(4), f(5)}
	for i, w := range want {
		if *gys[0][i] != *w {
			t.Fatalf("y[%d] = %v, want %v", i, *gys[0][i], *w)
		}
	}
	gx, gys, _ = downsample([]float64{0, 1, 2, 3}, [][]*float64{{f(1), f(1), nil, nil}}, 2)
	if len(gx) != 2 || gys[0][1] != nil {
		t.Fatalf("an empty group must be a gap: %v", gys[0])
	}
}

// For any series and any limit: at most limit points, x strictly increasing, and the
// global maximum preserved exactly.
func TestDownsampleProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 5000).Draw(t, "n")
		limit := rapid.IntRange(1, 2000).Draw(t, "limit")
		x := make([]float64, n)
		y := make([]*float64, n)
		peak := math.Inf(-1)
		for i := range n {
			x[i] = float64(i) * 0.5
			if rapid.IntRange(0, 9).Draw(t, "gap") > 0 {
				v := rapid.Float64Range(0, 1e6).Draw(t, "v")
				y[i] = &v
				peak = math.Max(peak, v)
			}
		}
		gx, gys, _ := downsample(x, [][]*float64{y}, limit)
		if len(gx) > limit || len(gys[0]) != len(gx) {
			t.Fatalf("%d points for a limit of %d", len(gx), limit)
		}
		got := math.Inf(-1)
		for i, v := range gys[0] {
			if i > 0 && gx[i] <= gx[i-1] {
				t.Fatalf("x not increasing at %d", i)
			}
			if v != nil {
				got = math.Max(got, *v)
			}
		}
		if got != peak {
			t.Fatalf("peak %v became %v", peak, got)
		}
	})
}
