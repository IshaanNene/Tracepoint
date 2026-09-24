// Package render holds TracePoint's renderers, and the few formatting rules they
// share so a number reads the same in the terminal, the HTML report, Markdown and
// JUnit.
//
// Every renderer is a pure function of result.json: given the same document it
// produces byte-identical output, which is what makes golden-file tests meaningful and
// what lets `tracepoint report` reproduce exactly what a run printed.
package render

import (
	"fmt"
	"sort"
	"strconv"
)

// MS renders a millisecond figure at a resolution a reader can act on: more precision
// on small numbers, less on large ones.
func MS(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v < 1:
		return fmt.Sprintf("%.2fms", v)
	case v < 10:
		return fmt.Sprintf("%.1fms", v)
	case v < 1000:
		return fmt.Sprintf("%.0fms", v)
	case v < 60_000:
		return fmt.Sprintf("%.1fs", v/1000)
	default:
		return fmt.Sprintf("%.1fm", v/60_000)
	}
}

// Seconds renders an offset in seconds.
func Seconds(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%ds", int64(v))
	}
	return fmt.Sprintf("%.1fs", v)
}

// Number renders a number without trailing noise: integers as integers, anything else
// to three significant figures.
func Number(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 3, 64)
}

// Percent renders a ratio as a percentage.
func Percent(ratio float64) string {
	switch {
	case ratio == 0:
		return "0%"
	case ratio < 0.001:
		return "<0.1%"
	default:
		return strconv.FormatFloat(ratio*100, 'f', 1, 64) + "%"
	}
}

// Count is one entry of a counted map, such as an error class.
type Count struct {
	Key string
	N   int64
}

// SortedCounts orders a counted map worst first, then by key, so output is stable and
// the important entry leads.
func SortedCounts(m map[string]int64) []Count {
	out := make([]Count, 0, len(m))
	for k, n := range m {
		out = append(out, Count{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Key < out[j].Key
	})
	return out
}
