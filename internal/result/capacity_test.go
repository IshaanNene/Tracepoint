package result

import (
	"strings"
	"testing"
)

func TestCapacitySummary(t *testing.T) {
	db := "db"
	knee := 400.0
	cases := []struct {
		c    Capacity
		want []string
	}{
		{Capacity{Knob: "rate", Levels: []CapacityLevel{{Value: 300, OK: true}, {Value: 400, BreakReason: "db p99 90ms > 50ms", Culprit: &db}},
			Boundary: &Boundary{LastOK: 300, FirstBroken: 400, Stable: true}, Knee: &knee,
			USL: &USL{Fitted: true, R2: 0.98, PeakN: 31, PeakThroughput: 812}},
			[]string{"holds at 300/s and breaks at 400/s, confirmed", "the db tier broke first: db p99 90ms > 50ms", "climbing sharply at 400/s", "812/s at a concurrency of 31"}},
		{Capacity{Knob: "concurrency", Levels: []CapacityLevel{{Value: 64, OK: true}}, Boundary: &Boundary{LastOK: 64, Stable: true}},
			[]string{"up to the ceiling of 64 users; nothing broke"}},
		{Capacity{Knob: "rate", Levels: []CapacityLevel{{Value: 300}}, Boundary: &Boundary{LastOK: 250, FirstBroken: 300, Range: []float64{200, 300}}},
			[]string{"between 200/s and 300/s", "a range, not a point"}},
		{Capacity{Knob: "rate", Levels: []CapacityLevel{{Value: 50, BreakReason: "plateau"}}, Boundary: &Boundary{FirstBroken: 50, Stable: true}},
			[]string{"broke at the first level, 50/s", "At 50/s: plateau."}},
		{Capacity{Knob: "rate"}, []string{"ran no level"}},
	}
	for _, c := range cases {
		got := c.c.Summary()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("summary %q lacks %q", got, w)
			}
		}
	}
}
