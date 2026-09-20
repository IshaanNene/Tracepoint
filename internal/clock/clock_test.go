package clock_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var epoch = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func TestFakeAdvancesOnlyOnCommand(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	f.Advance(90 * time.Second)
	if got := f.Now(); !got.Equal(epoch.Add(90 * time.Second)) {
		t.Errorf("after Advance, Now() = %v", got)
	}
	// Time must not run backwards: every consumer assumes a monotonic run timeline.
	f.Set(epoch)
	if got := f.Now(); !got.Equal(epoch.Add(90 * time.Second)) {
		t.Errorf("clock moved backwards to %v", got)
	}
}

func TestFakeSleepUntilWakesAtTheDeadline(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	woke := make(chan time.Time, 1)
	go func() {
		if err := f.SleepUntil(context.Background(), epoch.Add(5*time.Second)); err != nil {
			t.Errorf("SleepUntil: %v", err)
		}
		woke <- f.Now()
	}()

	f.BlockUntil(1) // the sleeper has provably registered; no sleeping-and-hoping
	f.Advance(4 * time.Second)
	select {
	case <-woke:
		t.Fatal("woke before the deadline")
	case <-time.After(20 * time.Millisecond):
	}

	f.Advance(time.Second)
	select {
	case at := <-woke:
		if at.Before(epoch.Add(5 * time.Second)) {
			t.Errorf("woke at %v, before the deadline", at)
		}
	case <-time.After(time.Second):
		t.Fatal("did not wake at the deadline")
	}
}

func TestSleepUntilPastDeadlineReturnsImmediately(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]clock.Clock{"real": clock.New(), "fake": clock.NewFake(epoch)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			past := c.Now().Add(-time.Hour)
			if err := c.SleepUntil(context.Background(), past); err != nil {
				t.Errorf("SleepUntil(past) = %v, want nil", err)
			}
		})
	}
}

// A cancelled context must win even when the deadline has already passed, or a
// cancelled run could slip one more operation through.
func TestSleepUntilHonoursCancellationEvenWhenDue(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]clock.Clock{"real": clock.New(), "fake": clock.NewFake(epoch)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := c.SleepUntil(ctx, c.Now().Add(-time.Second)); !errors.Is(err, context.Canceled) {
				t.Errorf("SleepUntil(cancelled, past) = %v, want context.Canceled", err)
			}
		})
	}
}

func TestSleepUntilUnblocksOnCancellation(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.SleepUntil(ctx, epoch.Add(time.Hour)) }()

	f.BlockUntil(1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock the sleeper")
	}
	// The cancelled sleeper must deregister, or a long run would leak a waiter per
	// cancelled operation.
	if got := f.Waiters(); got != 0 {
		t.Errorf("waiters after cancellation = %d, want 0", got)
	}
}

func TestFakeTimerResetAndStop(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	timer := f.NewTimer(time.Second)

	f.Advance(999 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("timer fired early")
	default:
	}
	f.Advance(time.Millisecond)
	select {
	case <-timer.C():
	default:
		t.Fatal("timer did not fire at its deadline")
	}

	// Reuse, which is what the arrival loop does thousands of times a second.
	timer.Reset(2 * time.Second)
	f.Advance(2 * time.Second)
	select {
	case <-timer.C():
	default:
		t.Fatal("timer did not fire after Reset")
	}

	timer.Reset(time.Second)
	if !timer.Stop() {
		t.Error("Stop on an armed timer should report true")
	}
	f.Advance(time.Hour)
	select {
	case <-timer.C():
		t.Error("a stopped timer fired")
	default:
	}
}

func TestFakeTickerFiresPerPeriod(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	tk := f.NewTicker(time.Second)
	defer tk.Stop()

	got := 0
	// Advancing past several periods must deliver ticks rather than swallow them
	// silently, so a bucket-sealing loop cannot miss a bucket.
	f.Advance(3 * time.Second)
	for {
		select {
		case <-tk.C():
			got++
			continue
		default:
		}
		break
	}
	if got == 0 {
		t.Fatal("ticker did not fire across three periods")
	}

	tk.Stop()
	drain(tk.C())
	f.Advance(10 * time.Second)
	select {
	case <-tk.C():
		t.Error("a stopped ticker fired")
	default:
	}
}

func TestNewTickerRejectsNonPositivePeriod(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("NewTicker(0) should panic, matching time.NewTicker")
		}
	}()
	clock.NewFake(epoch).NewTicker(0)
}

// The fake clock is shared by every goroutine in an executor test, so concurrent use
// has to be safe under -race.
func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	const sleepers = 16

	var wg sync.WaitGroup
	wg.Add(sleepers)
	for i := range sleepers {
		go func() {
			defer wg.Done()
			_ = f.SleepUntil(context.Background(), epoch.Add(time.Duration(i+1)*time.Millisecond))
		}()
	}
	f.BlockUntil(sleepers)

	var advancers sync.WaitGroup
	advancers.Add(4)
	for range 4 {
		go func() {
			defer advancers.Done()
			for range 10 {
				f.Advance(time.Millisecond)
				_ = f.Now()
			}
		}()
	}
	advancers.Wait()
	wg.Wait()
}

func TestRealClockMeasuresElapsedTime(t *testing.T) {
	t.Parallel()
	c := clock.New()
	start := c.Now()
	if err := c.SleepUntil(context.Background(), start.Add(5*time.Millisecond)); err != nil {
		t.Fatalf("SleepUntil: %v", err)
	}
	if elapsed := c.Now().Sub(start); elapsed < 5*time.Millisecond {
		t.Errorf("elapsed = %v, want at least 5ms", elapsed)
	}
}

func drain(ch <-chan time.Time) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
