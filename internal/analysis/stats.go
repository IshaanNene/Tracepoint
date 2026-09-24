package analysis

import (
	"math"
	"sort"
)

// median returns the middle value, averaging the two middle values of an even-length
// slice. It copies before sorting so a caller's timeline order is never disturbed.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// mad is the median absolute deviation from m.
func mad(v []float64, m float64) float64 {
	dev := make([]float64, len(v))
	for i, x := range v {
		dev[i] = math.Abs(x - m)
	}
	return median(dev)
}

// ranks assigns 1-based ranks, averaging the ranks of tied values.
func ranks(v []float64) []float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })
	out := make([]float64, len(v))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}

// spearman is Spearman's rank correlation: Pearson's correlation of the ranks, which
// handles ties correctly. It returns 0 when either series is constant or too short to
// say anything, because "no evidence" must not read as evidence either way.
func spearman(x, y []float64) float64 {
	if len(x) != len(y) || len(x) < 2 {
		return 0
	}
	return pearson(ranks(x), ranks(y))
}

func pearson(x, y []float64) float64 {
	n := float64(len(x))
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= n
	my /= n
	var sxy, sxx, syy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0
	}
	r := sxy / math.Sqrt(sxx*syy)
	// Rounding can push a perfect correlation a hair past the bound.
	return math.Max(-1, math.Min(1, r))
}

// round2 trims a derived number to two decimals, so the same data always renders the
// same digits and golden files stay stable.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// round3 trims to three decimals, for latencies in milliseconds.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
