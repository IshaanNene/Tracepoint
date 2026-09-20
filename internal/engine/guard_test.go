package engine_test

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/engine"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// feed drives the guard with a mix of outcomes over a span, at a steady rate.
func feed(g *engine.AbortGuard, class, okClass metrics.Class, failEvery int, from, to, step time.Duration) {
	i := 0
	for at := from; at < to; at += step {
		c := okClass
		if failEvery > 0 && i%failEvery == 0 {
			c = class
		}
		g.Observe(c, at)
		i++
	}
}

func TestGuardTripsOnSustainedFailure(t *testing.T) {
	t.Parallel()
	g := engine.NewAbortGuard(true, 0.5, 10*time.Second)

	// Everything fails, for longer than the window.
	feed(g, metrics.ClassHTTP5xx, metrics.ClassHTTP5xx, 1, 0, 12*time.Second, 20*time.Millisecond)

	tripped, reason := g.Tripped()
	if !tripped {
		t.Fatal("the guard did not trip on a fully failing target")
	}
	if !strings.Contains(reason, "10s") {
		t.Errorf("reason = %q, want it to name the window", reason)
	}
}

// A guard that fires before its window is full would abort on the handful of failures
// that happen while a connection pool warms up.
func TestGuardWaitsForAFullWindow(t *testing.T) {
	t.Parallel()
	g := engine.NewAbortGuard(true, 0.5, 10*time.Second)
	feed(g, metrics.ClassHTTP5xx, metrics.ClassHTTP5xx, 1, 0, 9*time.Second, 20*time.Millisecond)
	if tripped, _ := g.Tripped(); tripped {
		t.Error("the guard tripped before its window had elapsed")
	}
}

func TestGuardIgnoresHealthyRuns(t *testing.T) {
	t.Parallel()
	g := engine.NewAbortGuard(true, 0.5, 10*time.Second)
	// One failure in ten, sustained: bad, but nowhere near the threshold.
	feed(g, metrics.ClassHTTP5xx, metrics.ClassOK, 10, 0, 30*time.Second, 20*time.Millisecond)
	if tripped, reason := g.Tripped(); tripped {
		t.Errorf("the guard tripped at a 10%% failure rate: %s", reason)
	}
}

// A 4xx is the target working as designed, and a 429 is a rate limiter doing its job.
// Aborting on either would stop a run that was measuring exactly what it meant to.
func TestGuardIgnoresClientErrorsAndRateLimits(t *testing.T) {
	t.Parallel()
	for _, class := range []metrics.Class{metrics.ClassHTTP4xx, metrics.ClassExpectFailed, metrics.ClassPoolTimeout, metrics.ClassCanceled} {
		t.Run(class.String(), func(t *testing.T) {
			t.Parallel()
			g := engine.NewAbortGuard(true, 0.5, 10*time.Second)
			feed(g, class, class, 1, 0, 30*time.Second, 20*time.Millisecond)
			if tripped, reason := g.Tripped(); tripped {
				t.Errorf("%s tripped the guard: %s", class, reason)
			}
		})
	}
}

func TestGuardCountsTheRightFailures(t *testing.T) {
	t.Parallel()
	for _, class := range []metrics.Class{metrics.ClassHTTP5xx, metrics.ClassTimeout, metrics.ClassConnection, metrics.ClassDNS, metrics.ClassTLS} {
		t.Run(class.String(), func(t *testing.T) {
			t.Parallel()
			g := engine.NewAbortGuard(true, 0.5, 10*time.Second)
			feed(g, class, class, 1, 0, 12*time.Second, 20*time.Millisecond)
			if tripped, _ := g.Tripped(); !tripped {
				t.Errorf("%s did not trip the guard", class)
			}
		})
	}
}

// The window is trailing, not cumulative: failures age out of it.
//
// The threshold is "more than half of the last N seconds failed", not "half of the run
// so far". A burst small enough to stay under half the window must never trip, however
// total it was while it lasted.
func TestGuardWindowRolls(t *testing.T) {
	t.Parallel()

	t.Run("a burst under half the window ages out", func(t *testing.T) {
		t.Parallel()
		g := engine.NewAbortGuard(true, 0.5, 5*time.Second)
		// Two seconds of total failure, then recovery. Once the window is full it holds
		// at most 2s of failure against 3s of success: 40%, under the threshold.
		feed(g, metrics.ClassHTTP5xx, metrics.ClassHTTP5xx, 1, 0, 2*time.Second, 20*time.Millisecond)
		feed(g, metrics.ClassOK, metrics.ClassOK, 0, 2*time.Second, 30*time.Second, 20*time.Millisecond)
		if tripped, reason := g.Tripped(); tripped {
			t.Errorf("a 2s burst in a 5s window tripped the guard: %s", reason)
		}
	})

	t.Run("a burst filling most of the window does trip", func(t *testing.T) {
		t.Parallel()
		g := engine.NewAbortGuard(true, 0.5, 5*time.Second)
		// Four seconds of total failure inside a five-second window really is 80% of
		// that window, and the target really was failing for four seconds.
		feed(g, metrics.ClassHTTP5xx, metrics.ClassHTTP5xx, 1, 0, 4*time.Second, 20*time.Millisecond)
		feed(g, metrics.ClassOK, metrics.ClassOK, 0, 4*time.Second, 6*time.Second, 20*time.Millisecond)
		if tripped, _ := g.Tripped(); !tripped {
			t.Error("four seconds of total failure in a five-second window did not trip the guard")
		}
	})
}

func TestGuardCanBeDisabled(t *testing.T) {
	t.Parallel()
	g := engine.NewAbortGuard(false, 0.5, 10*time.Second)
	feed(g, metrics.ClassHTTP5xx, metrics.ClassHTTP5xx, 1, 0, 60*time.Second, 20*time.Millisecond)
	if tripped, _ := g.Tripped(); tripped {
		t.Error("a disabled guard tripped")
	}
}

// A handful of operations is not evidence of anything, however they went.
func TestGuardNeedsEnoughSamples(t *testing.T) {
	t.Parallel()
	g := engine.NewAbortGuard(true, 0.5, 10*time.Second)
	// Five failures spread over a long run: the window is full, the sample is not.
	for i := range 5 {
		g.Observe(metrics.ClassHTTP5xx, time.Duration(i)*time.Second+11*time.Second)
	}
	if tripped, _ := g.Tripped(); tripped {
		t.Error("the guard tripped on five observations")
	}
}

func TestGuardStaysTrippedOnce(t *testing.T) {
	t.Parallel()
	g := engine.NewAbortGuard(true, 0.5, 5*time.Second)
	feed(g, metrics.ClassHTTP5xx, metrics.ClassHTTP5xx, 1, 0, 7*time.Second, 20*time.Millisecond)
	if tripped, _ := g.Tripped(); !tripped {
		t.Fatal("the guard did not trip")
	}
	// Recovery afterwards must not un-trip it: the run is already ending.
	feed(g, metrics.ClassOK, metrics.ClassOK, 0, 7*time.Second, 60*time.Second, 20*time.Millisecond)
	if tripped, _ := g.Tripped(); !tripped {
		t.Error("the guard un-tripped after the target recovered")
	}
}

func TestGuardDefaults(t *testing.T) {
	t.Parallel()
	g := engine.NewAbortGuard(true, 0, 0)
	// The defaults are 50% over 10s, so 12 seconds of total failure must trip it.
	feed(g, metrics.ClassTimeout, metrics.ClassTimeout, 1, 0, 12*time.Second, 20*time.Millisecond)
	if tripped, _ := g.Tripped(); !tripped {
		t.Error("the default guard did not trip")
	}
}
