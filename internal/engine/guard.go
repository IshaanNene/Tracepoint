package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/metrics"
)

// Abort guard defaults (spec §8).
const (
	DefaultAbortRatio  = 0.5
	DefaultAbortWindow = 10 * time.Second
	// guardResolution is how finely the rolling window is divided. Fine enough to react
	// promptly, coarse enough that the ring stays small.
	guardResolution = 250 * time.Millisecond
)

// AbortGuard stops a run when the target is clearly failing.
//
// It exists because a load test is often unattended: a deployment breaks, and without
// a guard the generator keeps hammering a service that is already down for as long as
// the configured duration. Hammering something that is failing produces no measurement
// and makes the outage worse.
//
// What counts is deliberately narrow. Connection errors, timeouts and 5xx mean the
// target is failing. A 4xx does not - it is the target working as designed, correctly
// rejecting a request - and neither does a 429, which is a rate limiter doing its job.
// Aborting on those would stop a run that was measuring exactly what it meant to.
// Generator-side classes are excluded too: our own pool running dry says nothing about
// the target's health.
type AbortGuard struct {
	enabled bool
	ratio   float64
	window  time.Duration

	mu      sync.Mutex
	slots   []slot
	tripped bool
	reason  string
}

type slot struct {
	at     time.Duration
	total  int64
	failed int64
}

// NewAbortGuard builds a guard. A window of zero disables it.
func NewAbortGuard(enabled bool, ratio float64, window time.Duration) *AbortGuard {
	if ratio <= 0 {
		ratio = DefaultAbortRatio
	}
	if window <= 0 {
		window = DefaultAbortWindow
	}
	n := int(window/guardResolution) + 1
	return &AbortGuard{
		enabled: enabled,
		ratio:   ratio,
		window:  window,
		slots:   make([]slot, n),
	}
}

// Observe records one outcome at an offset from the run start.
func (g *AbortGuard) Observe(class metrics.Class, at time.Duration) {
	if !g.enabled {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tripped {
		return
	}

	idx := int(at/guardResolution) % len(g.slots)
	bucketAt := at.Truncate(guardResolution)
	if g.slots[idx].at != bucketAt {
		// The ring has come round; this slot holds an older period and is reused.
		g.slots[idx] = slot{at: bucketAt}
	}
	g.slots[idx].total++
	if class.CountsTowardAbortGuard() {
		g.slots[idx].failed++
	}

	g.evaluateLocked(at)
}

// evaluateLocked decides whether the failure rate has held above the threshold for the
// whole window.
//
// The window must be full before the guard can trip. Without that, the first three
// operations of a run all failing - which happens routinely while a pool warms up -
// would abort at a 100% failure rate over a third of a second.
func (g *AbortGuard) evaluateLocked(now time.Duration) {
	if now < g.window {
		return
	}
	cutoff := now - g.window

	var total, failed int64
	for _, s := range g.slots {
		if s.at < cutoff || s.total == 0 {
			continue
		}
		total += s.total
		failed += s.failed
	}
	// A handful of operations is not evidence of anything, however they went.
	const minSamples = 20
	if total < minSamples {
		return
	}
	if ratio := float64(failed) / float64(total); ratio > g.ratio {
		g.tripped = true
		g.reason = fmt.Sprintf(
			"%.0f%% of operations failed with connection errors, timeouts or 5xx over the last %s (%d of %d)",
			ratio*100, g.window, failed, total)
	}
}

// Tripped reports whether the guard has fired, and why.
func (g *AbortGuard) Tripped() (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tripped, g.reason
}

// guardedRecorder passes every outcome to the collector and to the guard.
//
// Wrapping the recorder rather than reaching into the collector keeps the guard out of
// the measurement path's data structures: the collector does not know it exists, and
// the guard sees exactly what was recorded.
type guardedRecorder struct {
	inner metrics.Recorder
	guard *AbortGuard
	// abort is called once, the first time the guard trips.
	abort func()
	once  sync.Once
}

func (g *guardedRecorder) Record(o *metrics.Outcome) {
	g.inner.Record(o)
	g.guard.Observe(o.Class, o.End)
	if tripped, _ := g.guard.Tripped(); tripped {
		g.once.Do(g.abort)
	}
}
