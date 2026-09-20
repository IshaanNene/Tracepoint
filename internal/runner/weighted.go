package runner

import (
	"math/rand/v2"
	"sort"
)

// Weighted picks an index in proportion to its weight.
//
// It is on the hot path once per operation, so it holds a prefix-sum table and binary
// searches it: no allocation, no map, and the same answer for the same random source,
// which is what makes a seeded run reproducible.
type Weighted struct {
	cumulative []float64
	total      float64
}

// NewWeighted builds a picker. Weights must be positive; validation has already
// rejected anything else by the time this runs.
func NewWeighted(weights []float64) *Weighted {
	w := &Weighted{cumulative: make([]float64, len(weights))}
	for i, x := range weights {
		if x <= 0 {
			x = 0
		}
		w.total += x
		w.cumulative[i] = w.total
	}
	return w
}

// Pick returns an index, or 0 when there is nothing to choose between.
func (w *Weighted) Pick(rng *rand.Rand) int {
	if len(w.cumulative) == 0 {
		return 0
	}
	if len(w.cumulative) == 1 || w.total <= 0 {
		return 0
	}
	target := rng.Float64() * w.total
	i := sort.SearchFloat64s(w.cumulative, target)
	if i >= len(w.cumulative) {
		i = len(w.cumulative) - 1
	}
	return i
}

// Len is how many choices there are.
func (w *Weighted) Len() int { return len(w.cumulative) }
