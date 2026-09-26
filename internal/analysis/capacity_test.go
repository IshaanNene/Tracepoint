package analysis

import (
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// capacityRun lays levels over a scenario: each level's measured window and whether
// it held.
func capacityRun(s *scenario, levels ...result.CapacityLevel) *result.Result {
	r := s.result()
	r.Capacity = &result.Capacity{Knob: "rate", Runner: "http", Levels: levels}
	return r
}

func level(value float64, from, to int, ok bool) result.CapacityLevel {
	return result.CapacityLevel{Value: value, StartIndex: from, EndIndex: to, OK: ok}
}

func culpritOf(l result.CapacityLevel) string {
	if l.Culprit == nil {
		return "<nil>"
	}
	return *l.Culprit
}

// Each broken level is attributed by the same analysis a whole run gets, over the
// timeline up to the level's end: the tier whose incident overlaps the level.
func TestAttributeLevels(t *testing.T) {
	s := newScenario(120).
		set("db", 70, 89, 40).set("http", 71, 89, 400). // the db leads, the app follows
		set("http", 100, 119, 400)                      // only the app
	r := capacityRun(s,
		level(50, 10, 29, true), level(100, 40, 59, true),
		level(200, 70, 89, false), level(150, 100, 119, false),
		level(120, 60, 65, false), // broke, but nothing went hot inside it
	)
	AttributeLevels(r, Inputs{})
	got := []string{}
	for _, l := range r.Capacity.Levels {
		got = append(got, culpritOf(l))
	}
	want := []string{"<nil>", "<nil>", "db", "http", "<nil>"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("culprits = %v, want %v", got, want)
		}
	}
	// The run's own buckets are untouched by the attribution.
	if n := len(r.Runners[0].Buckets); n != 120 {
		t.Fatalf("the result lost buckets: %d", n)
	}
}

func TestAttributeLevelsWithoutASearch(t *testing.T) {
	r := newScenario(30).result()
	AttributeLevels(r, Inputs{}) // no capacity section: nothing to do, and no panic
	if r.Capacity != nil {
		t.Fatal("a capacity section appeared")
	}
}
