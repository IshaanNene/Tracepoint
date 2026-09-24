package analysis

import (
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// ramp is a noisy upward series, so ranks are well defined.
func ramp(n int, base float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = base * (1 + float64(i)/10 + 0.01*float64((i*7)%5))
	}
	return out
}

func viewOf(name, kind string, vals []float64) *view {
	r := &result.Runner{Name: name, Kind: kind, Buckets: series(vals...)}
	return newView(r, 100, nil)
}

func TestCorrelateLagZero(t *testing.T) {
	c := correlate(viewOf("http", "app", ramp(40, 20)), viewOf("db", "storage", ramp(40, 2)), DefaultParams())
	if c.Rho < 0.99 || c.BestLag != 0 || c.Lean != LeanStorage {
		t.Fatalf("got %+v, want rho 1 at lag 0 leaning storage", c)
	}
	if len(c.RhoByLag) != 7 {
		t.Fatalf("rho_by_lag has %d lags, want 7", len(c.RhoByLag))
	}
}

// A storage series that leads the application by two buckets is found at lag +2.
func TestCorrelateStorageLeads(t *testing.T) {
	n := 60
	pattern := make([]float64, n+2)
	for i := range pattern {
		pattern[i] = 10 + float64((i*37)%23) // a scrambled but deterministic sequence
	}
	app := make([]float64, n)
	db := make([]float64, n)
	for i := range n {
		db[i] = pattern[i+2] / 5
		app[i] = pattern[i] * 3 // app at i equals db at i-2
	}
	c := correlate(viewOf("http", "app", app), viewOf("db", "storage", db), DefaultParams())
	if c.BestLag != 2 || c.Rho < 0.99 {
		t.Fatalf("got best lag %d rho %v, want +2 and 1", c.BestLag, c.Rho)
	}
	if c.Lean != LeanStorage {
		t.Fatalf("lean = %s, want storage", c.Lean)
	}

	// Swap the roles: the application leads, so storage is following it.
	c = correlate(viewOf("http", "app", db), viewOf("db", "storage", app), DefaultParams())
	if c.BestLag != -2 || c.Lean != LeanApp {
		t.Fatalf("got %+v, want lag -2 leaning app", c)
	}
}

func TestCorrelateTooFewPairs(t *testing.T) {
	c := correlate(viewOf("http", "app", ramp(8, 20)), viewOf("db", "storage", ramp(8, 2)), DefaultParams())
	if c.Lean != LeanNone {
		t.Fatalf("lean = %s with %d pairs, want none", c.Lean, c.NBuckets)
	}
}

func TestCorrelateSkipsIneligible(t *testing.T) {
	app := viewOf("http", "app", ramp(30, 20))
	db := viewOf("db", "storage", ramp(30, 2))
	for i := range 10 {
		db.runner.Buckets[i].Insufficient = true
	}
	db = newView(db.runner, 100, nil)
	c := correlate(app, db, DefaultParams())
	if c.NBuckets != 20 {
		t.Fatalf("n = %d, want 20 eligible pairs", c.NBuckets)
	}
}

func TestLagOrder(t *testing.T) {
	got := lagOrder(3)
	want := []int{0, 1, -1, 2, -2, 3, -3}
	if !equalInts(got, want) {
		t.Fatalf("lagOrder = %v, want %v", got, want)
	}
}
