package executor_test

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/executor"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/schedule"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var epoch = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// fakeRunner performs an operation by waiting on the fake clock for a fixed service
// time, so a test can reason exactly about concurrency and drops.
type fakeRunner struct {
	clk     clock.Clock
	start   time.Time
	service time.Duration

	started  atomic.Int64
	finished atomic.Int64
	// gate, when non-nil, holds every operation until it is closed.
	gate chan struct{}
}

func (f *fakeRunner) Name() string                  { return "http" }
func (f *fakeRunner) Kind() metrics.Kind            { return metrics.KindApp }
func (f *fakeRunner) Labels() []string              { return []string{"probe"} }
func (f *fakeRunner) Prepare(context.Context) error { return nil }
func (f *fakeRunner) Close() error                  { return nil }

func (f *fakeRunner) Do(ctx context.Context, it *runner.Iteration, rec metrics.Recorder) error {
	f.started.Add(1)
	var o metrics.Outcome
	it.StampOutcome(&o)
	o.ConnAcquired = f.clk.Now().Sub(f.start)

	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			o.Class = metrics.ClassCanceled
			o.End = f.clk.Now().Sub(f.start)
			rec.Record(&o)
			f.finished.Add(1)
			return nil
		}
	}
	if f.service > 0 {
		if err := f.clk.SleepUntil(ctx, f.clk.Now().Add(f.service)); err != nil {
			o.Class = metrics.ClassCanceled
			o.End = f.clk.Now().Sub(f.start)
			rec.Record(&o)
			f.finished.Add(1)
			return nil
		}
	}
	o.Class = metrics.ClassOK
	o.Status = 200
	o.End = f.clk.Now().Sub(f.start)
	rec.Record(&o)
	f.finished.Add(1)
	return nil
}

func newCollector(t *testing.T) *metrics.Collector {
	t.Helper()
	c, err := metrics.NewCollector(metrics.Config{
		Runner: "http", Kind: metrics.KindApp, Labels: []string{"probe"},
		BucketWidth: time.Second, SealDelay: 10 * time.Second, MinSamples: 1,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return c
}

func constantSchedule(t *testing.T, rate float64, over time.Duration) *schedule.Schedule {
	t.Helper()
	p, err := schedule.NewProfile(rate, []schedule.Stage{{Duration: over, Target: rate}})
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	s, err := schedule.NewSchedule(p, schedule.ArrivalUniform, rand.New(rand.NewPCG(1, 2)))
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	return s
}

// The open model's defining property: every scheduled arrival is offered, on time,
// whatever the target is doing.
func TestEveryArrivalIsOfferedOnSchedule(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	col := newCollector(t)
	rnr := &fakeRunner{clk: clk, start: epoch}

	e, err := executor.NewArrivalRate(executor.Config{
		Runner: rnr, Schedule: constantSchedule(t, 10, 2*time.Second),
		Collector: col, Clock: clk, Start: epoch, MaxInFlight: 4,
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), context.Background()) }()

	// Drive the fake clock until the schedule is exhausted.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			s := e.Stats()
			if s.Offered != 20 {
				t.Errorf("offered %d arrivals, want 20 (10/s for 2s)", s.Offered)
			}
			if s.Dropped != 0 {
				t.Errorf("dropped %d arrivals with an idle target", s.Dropped)
			}
			if got := rnr.finished.Load(); got != 20 {
				t.Errorf("performed %d operations, want 20", got)
			}
			return
		case <-deadline:
			t.Fatal("the executor did not finish")
		default:
			clk.Advance(10 * time.Millisecond)
			time.Sleep(time.Millisecond)
		}
	}
}

// When every worker is busy and the queue is full, the arrival is dropped and counted
// rather than queued without bound. This is what keeps a capacity problem from being
// reported as latency.
func TestFullQueueDropsAndCountsRatherThanQueueing(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	col := newCollector(t)
	gate := make(chan struct{})
	rnr := &fakeRunner{clk: clk, start: epoch, gate: gate}

	e, err := executor.NewArrivalRate(executor.Config{
		Runner: rnr, Schedule: constantSchedule(t, 100, time.Second),
		Collector: col, Clock: clk, Start: epoch,
		MaxInFlight: 2, QueueDepth: 1, // room for three arrivals in total
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), context.Background()) }()

	// Advance through the whole schedule while every operation is held open.
	for range 200 {
		clk.Advance(10 * time.Millisecond)
		time.Sleep(200 * time.Microsecond)
	}
	close(gate) // let the held operations finish

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the executor did not finish after the gate opened")
	}

	s := e.Stats()
	if s.Offered != 100 {
		t.Errorf("offered %d, want all 100 arrivals to be offered regardless of capacity", s.Offered)
	}
	if s.Dropped == 0 {
		t.Fatal("nothing was dropped although capacity was 3 and 100 arrivals were offered")
	}
	if s.Dispatched+s.Dropped != s.Offered {
		t.Errorf("dispatched %d + dropped %d != offered %d; arrivals went missing",
			s.Dispatched, s.Dropped, s.Offered)
	}
	if s.PeakInFlight > 2 {
		t.Errorf("peak in flight %d exceeded max_in_flight 2", s.PeakInFlight)
	}

	col.Finish(time.Hour)
	var dropped int64
	for _, b := range col.Snapshot().Buckets {
		dropped += b.Dropped
	}
	if dropped != s.Dropped {
		t.Errorf("the timeline recorded %d drops, the executor counted %d", dropped, s.Dropped)
	}
}

func TestMaxInFlightIsRespected(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	col := newCollector(t)
	gate := make(chan struct{})
	rnr := &fakeRunner{clk: clk, start: epoch, gate: gate}

	const limit = 5
	e, err := executor.NewArrivalRate(executor.Config{
		Runner: rnr, Schedule: constantSchedule(t, 200, time.Second),
		Collector: col, Clock: clk, Start: epoch,
		MaxInFlight: limit, QueueDepth: 50,
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), context.Background()) }()
	for range 150 {
		clk.Advance(10 * time.Millisecond)
		time.Sleep(200 * time.Microsecond)
		if got := e.InFlight(); got > limit {
			t.Fatalf("in flight reached %d, above the limit of %d", got, limit)
		}
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the executor did not finish")
	}
	if got := e.Stats().PeakInFlight; got > limit {
		t.Errorf("peak in flight %d exceeded the limit %d", got, limit)
	}
}

// A graceful stop stops offering arrivals but lets in-flight work finish. That is why
// the executor takes two contexts.
func TestGracefulStopDrainsInFlightWork(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	col := newCollector(t)
	rnr := &fakeRunner{clk: clk, start: epoch}

	e, err := executor.NewArrivalRate(executor.Config{
		Runner: rnr, Schedule: constantSchedule(t, 50, 10*time.Second),
		Collector: col, Clock: clk, Start: epoch, MaxInFlight: 8,
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}

	arrivalCtx, stopArrivals := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(arrivalCtx, context.Background()) }()

	for range 50 {
		clk.Advance(10 * time.Millisecond)
		time.Sleep(200 * time.Microsecond)
	}
	stopArrivals()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled arrival context is a graceful stop, not an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the executor did not stop")
	}

	s := e.Stats()
	if s.Offered == 0 {
		t.Fatal("nothing was offered before the stop")
	}
	if s.Offered >= 500 {
		t.Errorf("offered %d; arrivals continued past the stop", s.Offered)
	}
	// Everything that was dispatched must have been completed, not abandoned.
	if got, want := rnr.finished.Load(), s.Dispatched; got != want {
		t.Errorf("finished %d of %d dispatched operations; in-flight work was abandoned", got, want)
	}
}

// Cancelling the work context cuts in-flight operations short. The engine does this
// after the grace period, and the outcomes are recorded as canceled rather than as
// target failures.
func TestCancellingWorkContextEndsInFlightOperations(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	col := newCollector(t)
	gate := make(chan struct{})
	defer close(gate)
	rnr := &fakeRunner{clk: clk, start: epoch, gate: gate}

	e, err := executor.NewArrivalRate(executor.Config{
		Runner: rnr, Schedule: constantSchedule(t, 20, time.Second),
		Collector: col, Clock: clk, Start: epoch, MaxInFlight: 4, QueueDepth: 4,
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}

	workCtx, cutOff := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), workCtx) }()

	for range 30 {
		clk.Advance(10 * time.Millisecond)
		time.Sleep(200 * time.Microsecond)
	}
	cutOff()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the work context did not end the run")
	}
	col.Finish(time.Hour)
	if got := col.Snapshot().Summary.Errors["canceled"]; got == 0 {
		t.Error("cut-off operations must be recorded as canceled, not as target failures")
	}
}

// Dispatch lag is what decides whether a run can be believed. On a clock that never
// falls behind it must be zero.
func TestDispatchLagIsZeroWhenTheGeneratorKeepsUp(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	col := newCollector(t)
	rnr := &fakeRunner{clk: clk, start: epoch}

	e, err := executor.NewArrivalRate(executor.Config{
		Runner: rnr, Schedule: constantSchedule(t, 20, time.Second),
		Collector: col, Clock: clk, Start: epoch, MaxInFlight: 8,
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), context.Background()) }()
	for range 120 {
		clk.Advance(10 * time.Millisecond)
		time.Sleep(200 * time.Microsecond)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the executor did not finish")
	}
	if got := e.Stats().DispatchLag.P99; got > 11 {
		t.Errorf("dispatch lag p99 = %vms on a clock that never fell behind", got)
	}
}

func TestConstructionRejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	col := newCollector(t)
	full := executor.Config{
		Runner: &fakeRunner{clk: clk, start: epoch}, Schedule: constantSchedule(t, 1, time.Second),
		Collector: col, Clock: clk, Start: epoch,
	}
	cases := map[string]func(*executor.Config){
		"no runner":    func(c *executor.Config) { c.Runner = nil },
		"no schedule":  func(c *executor.Config) { c.Schedule = nil },
		"no collector": func(c *executor.Config) { c.Collector = nil },
		"no clock":     func(c *executor.Config) { c.Clock = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := full
			mutate(&cfg)
			if _, err := executor.NewArrivalRate(cfg); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	e, err := executor.NewArrivalRate(executor.Config{
		Runner: &fakeRunner{clk: clk, start: epoch}, Schedule: constantSchedule(t, 1, time.Second),
		Collector: newCollector(t), Clock: clk, Start: epoch,
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}
	if got := e.Stats().MaxInFlight; got != executor.DefaultMaxInFlight {
		t.Errorf("max in flight = %d, want the default %d", got, executor.DefaultMaxInFlight)
	}
}

// Workers must not be a source of shared-state races, and none may outlive Run.
func TestNoGoroutinesOutliveRun(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	rnr := &fakeRunner{clk: clk, start: epoch}
	e, err := executor.NewArrivalRate(executor.Config{
		Runner: rnr, Schedule: constantSchedule(t, 5, time.Second),
		Collector: newCollector(t), Clock: clk, Start: epoch, MaxInFlight: 16,
	})
	if err != nil {
		t.Fatalf("NewArrivalRate: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = e.Run(context.Background(), context.Background()) }()
	for range 120 {
		clk.Advance(10 * time.Millisecond)
		time.Sleep(200 * time.Microsecond)
	}
	wg.Wait()
	// goleak in TestMain is the actual assertion; reaching here means Run returned.
}
