// Package clock defines the time abstraction the rest of TracePoint uses.
//
// Nothing in the engine calls time.Now, time.Sleep or time.NewTimer directly. Every
// component takes a Clock, which makes the scheduler and the executors testable
// without waiting in real time: a Fake clock advances on command, so a three-minute
// ramp is exercised in microseconds and the result is deterministic.
//
// Durations measured through the real clock are monotonic. time.Time values obtained
// from Now carry a monotonic reading, so Sub between them is unaffected by wall-clock
// adjustments - which matters for a tool whose entire output is elapsed time.
package clock

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Clock is the source of time for a run.
type Clock interface {
	// Now returns the current time. Readings from the real clock carry a monotonic
	// component, so differences between them are immune to wall-clock changes.
	Now() time.Time

	// SleepUntil blocks until the deadline passes or ctx is done, returning ctx.Err()
	// in the latter case. A deadline already in the past returns immediately.
	SleepUntil(ctx context.Context, deadline time.Time) error

	// NewTimer returns a timer that fires once after d. Callers that wait repeatedly -
	// the arrival loop fires thousands of times a second - should reuse one timer
	// rather than allocating per wait.
	NewTimer(d time.Duration) Timer

	// NewTicker returns a ticker that fires every d. Used for bucket sealing,
	// telemetry sampling and progress output.
	NewTicker(d time.Duration) Ticker
}

// Timer fires once, and can be reset and reused.
type Timer interface {
	C() <-chan time.Time
	// Reset rearms the timer for d. As with time.Timer, drain C before resetting a
	// timer that may already have fired.
	Reset(d time.Duration) bool
	Stop() bool
}

// Ticker fires repeatedly until stopped.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Real is the production clock, backed by the time package.
type Real struct{}

// New returns the real clock.
func New() Real { return Real{} }

// Now returns the current time, with a monotonic reading.
func (Real) Now() time.Time { return time.Now() }

// SleepUntil blocks until the deadline or ctx, whichever comes first.
func (Real) SleepUntil(ctx context.Context, deadline time.Time) error {
	d := time.Until(deadline)
	if d <= 0 {
		// Still honour an already-cancelled context, so a cancelled run cannot slip
		// one more operation through on a deadline that happens to have passed.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewTimer returns a one-shot timer.
func (Real) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

// NewTicker returns a repeating ticker.
func (Real) NewTicker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time        { return r.t.C }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
func (r *realTimer) Stop() bool                 { return r.t.Stop() }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// Fake is a manually advanced clock for tests.
//
// It makes timing deterministic: a test sets up waiters, calls BlockUntil to be sure
// they have registered, then advances time and asserts on what fired. Nothing sleeps
// in real time, so a soak-length schedule runs instantly.
//
// Fake is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	// blocked is signalled whenever the waiter set changes, so BlockUntil can wait
	// for goroutines to arrive rather than sleeping and hoping.
	blocked *sync.Cond
}

type waiter struct {
	deadline time.Time
	ch       chan time.Time
	// period is non-zero for tickers, which rearm after firing.
	period time.Duration
	// stopped tickers and timers are dropped at the next advance.
	stopped bool
}

// NewFake returns a fake clock set to start.
//
// The zero time is deliberately avoided by callers passing a realistic instant; any
// instant works, because everything in a result is an offset from the run start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.blocked = sync.NewCond(&f.mu)
	return f
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d, firing everything scheduled at or before the
// new time, in deadline order. Tickers that fall several periods behind fire once per
// elapsed period, matching time.Ticker's behaviour of not coalescing silently beyond
// its one-slot buffer.
func (f *Fake) Advance(d time.Duration) { f.Set(f.Now().Add(d)) }

// Set moves the clock to t. Moving backwards is a programming error in a test and is
// ignored, because every consumer of this package assumes time does not reverse.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Before(f.now) {
		return
	}
	f.now = t
	f.fireLocked()
}

// fireLocked delivers to every waiter whose deadline has passed. Deliveries are
// non-blocking: a receiver that is not listening does not stall the clock, exactly as
// a real ticker drops ticks into a one-slot buffer.
func (f *Fake) fireLocked() {
	for {
		sort.SliceStable(f.waiters, func(i, j int) bool {
			return f.waiters[i].deadline.Before(f.waiters[j].deadline)
		})
		fired := false
		kept := f.waiters[:0]
		for _, w := range f.waiters {
			switch {
			case w.stopped:
				continue
			case !w.deadline.After(f.now):
				select {
				case w.ch <- w.deadline:
				default:
				}
				fired = true
				if w.period > 0 {
					w.deadline = w.deadline.Add(w.period)
					kept = append(kept, w)
				}
			default:
				kept = append(kept, w)
			}
		}
		f.waiters = kept
		if !fired {
			break
		}
	}
	f.blocked.Broadcast()
}

// Waiters reports how many timers, tickers and sleepers are currently registered.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntil waits until exactly n waiters are registered.
//
// This is what removes the race from a timing test: rather than sleeping and hoping
// the goroutine under test has reached its wait, the test blocks until it provably
// has, then advances the clock.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) != n {
		f.blocked.Wait()
	}
}

func (f *Fake) add(w *waiter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waiters = append(f.waiters, w)
	f.blocked.Broadcast()
}

func (f *Fake) remove(w *waiter) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.waiters {
		if x == w {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			f.blocked.Broadcast()
			return true
		}
	}
	w.stopped = true
	return false
}

// SleepUntil blocks until the fake clock reaches the deadline or ctx is done.
func (f *Fake) SleepUntil(ctx context.Context, deadline time.Time) error {
	f.mu.Lock()
	if !deadline.After(f.now) {
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	w := &waiter{deadline: deadline, ch: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	f.blocked.Broadcast()
	f.mu.Unlock()

	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		f.remove(w)
		return ctx.Err()
	}
}

// NewTimer returns a one-shot timer on the fake clock.
func (f *Fake) NewTimer(d time.Duration) Timer {
	w := &waiter{deadline: f.Now().Add(d), ch: make(chan time.Time, 1)}
	f.add(w)
	return &fakeTimer{f: f, w: w}
}

// NewTicker returns a repeating ticker on the fake clock. A non-positive period is a
// programming error and panics, matching time.NewTicker.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	w := &waiter{deadline: f.Now().Add(d), ch: make(chan time.Time, 1), period: d}
	f.add(w)
	return &fakeTicker{f: f, w: w}
}

type fakeTimer struct {
	f *Fake
	w *waiter
}

func (t *fakeTimer) C() <-chan time.Time { return t.w.ch }

func (t *fakeTimer) Reset(d time.Duration) bool {
	active := t.f.remove(t.w)
	t.w.stopped = false
	t.w.deadline = t.f.Now().Add(d)
	t.f.add(t.w)
	return active
}

func (t *fakeTimer) Stop() bool { return t.f.remove(t.w) }

type fakeTicker struct {
	f *Fake
	w *waiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }
func (t *fakeTicker) Stop()               { t.f.remove(t.w) }

// Compile-time proof that both clocks satisfy the interface.
var (
	_ Clock = Real{}
	_ Clock = (*Fake)(nil)
)
