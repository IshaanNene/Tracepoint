// Package executor drives arrivals into work.
//
// The open model (arrival-rate) fires arrivals on a schedule regardless of whether
// earlier ones have finished, which is what makes latency coordinated-omission
// correct: a target that slows down cannot reduce the load offered to it, so its
// stalls are measured rather than hidden. The closed model (vus) arrives in phase 6.
//
// When every worker is busy and the dispatch queue is full, an arrival is dropped and
// counted - never queued without bound. An unbounded queue would turn a capacity
// problem into a memory problem and report our own backlog as the target's latency.
// See docs/adr/003-executors-and-desugaring.md.
package executor

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/schedule"
)

// Defaults for an arrival-rate executor.
const (
	DefaultMaxInFlight = 256
	DefaultQueueDepth  = 64
)

// Config configures an arrival-rate executor.
type Config struct {
	Runner    runner.Runner
	Schedule  *schedule.Schedule
	Collector *metrics.Collector
	// Recorder is what outcomes are written to. It defaults to Collector; the engine
	// supplies a wrapper so the abort guard sees every outcome on its way through.
	Recorder metrics.Recorder
	Clock    clock.Clock

	// Start is the single monotonic instant every runner and sampler shares. Arrival
	// offsets are measured from it.
	Start time.Time

	MaxInFlight int
	QueueDepth  int
	Seed        uint64
	Logger      *slog.Logger
}

// Stats is what the executor observed, as opposed to what it was asked for. The gap
// between Offered and Dispatched is the first thing to check when a run looks wrong.
type Stats struct {
	Offered      int64
	Dispatched   int64
	Dropped      int64
	PeakInFlight int64
	MaxInFlight  int
	// DispatchLag is the distribution of how late arrivals were handed out relative to
	// their scheduled time. A high p99 here means the generator missed its own
	// schedule, which makes the run invalid rather than merely slow.
	DispatchLag metrics.Quantiles
}

// ArrivalRate is the open-model executor.
type ArrivalRate struct {
	cfg Config

	queue        chan job
	inFlight     atomic.Int64
	peakInFlight atomic.Int64
	offered      atomic.Int64
	dispatched   atomic.Int64
	dropped      atomic.Int64

	lagMu sync.Mutex
	lag   *lagTracker
}

type job struct {
	index      int64
	intended   time.Duration
	dispatched time.Duration
}

// NewArrivalRate builds an open-model executor.
func NewArrivalRate(cfg Config) (*ArrivalRate, error) {
	switch {
	case cfg.Runner == nil:
		return nil, errs.New(errs.CodeInternal, "an executor needs a runner")
	case cfg.Schedule == nil:
		return nil, errs.New(errs.CodeInternal, "an executor needs a schedule")
	case cfg.Collector == nil:
		return nil, errs.New(errs.CodeInternal, "an executor needs a collector")
	case cfg.Clock == nil:
		return nil, errs.New(errs.CodeInternal, "an executor needs a clock")
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = DefaultMaxInFlight
	}
	if cfg.QueueDepth < 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Recorder == nil {
		cfg.Recorder = cfg.Collector
	}
	lag, err := newLagTracker()
	if err != nil {
		return nil, err
	}
	return &ArrivalRate{
		cfg:   cfg,
		queue: make(chan job, cfg.QueueDepth),
		lag:   lag,
	}, nil
}

// Run generates arrivals and performs them.
//
// Two contexts, deliberately. arrivalCtx stops the schedule: when it is done, no
// further arrivals are offered. workCtx bounds the work itself, so the engine can let
// in-flight operations drain for a grace period after arrivals stop and only then cut
// them off. Collapsing them into one would mean a graceful stop either abandoned
// in-flight work or waited forever.
func (e *ArrivalRate) Run(arrivalCtx, workCtx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(e.cfg.MaxInFlight)
	for i := range e.cfg.MaxInFlight {
		go func(worker int) {
			defer wg.Done()
			e.work(workCtx, worker)
		}(i)
	}

	// Arrivals stop when either context is done. Cancelling the work context means
	// in-flight operations are being cut off, and offering further arrivals that would
	// be cancelled the moment they started would inflate the offered count with load
	// the target never saw.
	genCtx, stopGenerating := context.WithCancel(arrivalCtx)
	defer stopGenerating()
	stopWatching := context.AfterFunc(workCtx, stopGenerating)
	defer stopWatching()

	err := e.generate(genCtx)
	close(e.queue)
	wg.Wait()
	return err
}

// generate walks the schedule, sleeping until each arrival is due and offering it to
// the workers.
func (e *ArrivalRate) generate(ctx context.Context) error {
	for {
		at, ok := e.cfg.Schedule.Next()
		if !ok {
			return nil // the profile is exhausted: a normal end
		}
		if err := e.cfg.Clock.SleepUntil(ctx, e.cfg.Start.Add(at)); err != nil {
			// Cancellation is how a graceful stop and a signal both arrive. It is not
			// a failure, so the run keeps whatever it has measured.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}

		now := e.elapsed()
		bucket := e.cfg.Collector.BucketIndexOf(at)
		e.offered.Add(1)
		e.cfg.Collector.RecordOffered(bucket)

		lag := now - at
		if lag < 0 {
			lag = 0
		}
		e.recordLag(lag)

		select {
		case e.queue <- job{index: e.offered.Load(), intended: at, dispatched: now}:
			e.dispatched.Add(1)
		default:
			// Every worker is busy and the queue is full. Drop it and say so: holding
			// it would inflate response time without bound and report our own backlog
			// as the target's latency.
			e.dropped.Add(1)
			e.cfg.Collector.RecordDropped(bucket)
		}
	}
}

// work is one worker's loop. It drains the queue to completion even after arrivals
// stop, which is what makes the graceful drain graceful.
func (e *ArrivalRate) work(ctx context.Context, worker int) {
	// Each worker owns its random source so weighted picks never contend, seeded from
	// the run seed and the worker index so the whole run stays reproducible.
	rng := rand.New(rand.NewPCG(e.cfg.Seed, uint64(worker)+1)) //nolint:gosec // reproducibility, not secrecy

	for j := range e.queue {
		n := e.inFlight.Add(1)
		e.notePeak(n)
		e.cfg.Collector.ObserveInFlight(e.cfg.Collector.BucketIndexOf(j.intended), n)

		it := runner.Iteration{
			Index:       j.index,
			Worker:      worker,
			Intended:    j.intended,
			Dispatched:  j.dispatched,
			WorkerStart: e.elapsed(),
			Rand:        rng,
		}
		if err := e.cfg.Runner.Do(ctx, &it, e.cfg.Recorder); err != nil {
			// An operation that merely failed is recorded by the runner as an error
			// outcome. Reaching here means the runner could not even attempt it, which
			// is worth a line in the log but is not a reason to stop the run.
			e.cfg.Logger.Debug("runner could not attempt an operation",
				"runner", e.cfg.Runner.Name(), "worker", worker, "err", err)
		}
		e.inFlight.Add(-1)
	}
}

func (e *ArrivalRate) elapsed() time.Duration { return e.cfg.Clock.Now().Sub(e.cfg.Start) }

func (e *ArrivalRate) notePeak(n int64) {
	for {
		cur := e.peakInFlight.Load()
		if n <= cur || e.peakInFlight.CompareAndSwap(cur, n) {
			return
		}
	}
}

func (e *ArrivalRate) recordLag(d time.Duration) {
	e.lagMu.Lock()
	e.lag.add(d)
	e.lagMu.Unlock()
}

// Stats reports what the executor did. Safe to call after Run returns.
func (e *ArrivalRate) Stats() Stats {
	e.lagMu.Lock()
	lag := e.lag.quantiles()
	e.lagMu.Unlock()
	return Stats{
		Offered:      e.offered.Load(),
		Dispatched:   e.dispatched.Load(),
		Dropped:      e.dropped.Load(),
		PeakInFlight: e.peakInFlight.Load(),
		MaxInFlight:  e.cfg.MaxInFlight,
		DispatchLag:  lag,
	}
}

// InFlight is the current number of operations in progress, for live progress output.
func (e *ArrivalRate) InFlight() int64 { return e.inFlight.Load() }
