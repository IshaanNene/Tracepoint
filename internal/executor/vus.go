package executor

import (
	"context"
	"log/slog"
	"math"
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

// Executor is what the engine drives: the open model or the closed one.
type Executor interface {
	// Run generates work until the profile ends or arrivalCtx is done, then lets
	// what is in flight finish on workCtx.
	Run(arrivalCtx, workCtx context.Context) error
	Stats() Stats
	InFlight() int64
	// RecentLag is the dispatch lag p99 since the previous call; ok is false when
	// there is none to report.
	RecentLag() (p99 float64, ok bool)
}

var (
	_ Executor = (*ArrivalRate)(nil)
	_ Executor = (*VUs)(nil)
)

// vuTick is how often the closed model re-reads its target VU count.
const vuTick = 100 * time.Millisecond

// VUsConfig configures the closed model.
type VUsConfig struct {
	Runner    runner.Runner
	Profile   *schedule.Profile // read as a VU count over time
	Collector *metrics.Collector
	Recorder  metrics.Recorder
	Clock     clock.Clock
	Start     time.Time
	// PerVU pins each user to its first weighted choice (pick: per-vu).
	PerVU bool
	// Grace is how long users still mid-iteration when the profile ends may take to
	// finish before they are cut off.
	Grace  time.Duration
	Seed   uint64
	Logger *slog.Logger
}

// VUs is the closed-model executor (ADR-003): a population of virtual users, each
// looping one iteration after another as fast as the target answers.
//
// Offered load is an output here, not an input: when the target slows, each user
// waits for its answer, and the population offers less, as real users with
// sequential sessions would. There is no schedule, so there is nothing to be late
// against: an iteration is due when its user starts it, response time and service
// time converge, and client wait is only pool and connection waits. That is a
// property of the model, and the methodology says so.
type VUs struct {
	cfg VUsConfig

	iterations atomic.Int64
	active     atomic.Int64
	peak       atomic.Int64
	maxVUs     atomic.Int64
}

// NewVUs builds a closed-model executor.
func NewVUs(cfg VUsConfig) (*VUs, error) {
	switch {
	case cfg.Runner == nil:
		return nil, errs.New(errs.CodeInternal, "an executor needs a runner")
	case cfg.Profile == nil:
		return nil, errs.New(errs.CodeInternal, "a vus executor needs a profile")
	case cfg.Collector == nil:
		return nil, errs.New(errs.CodeInternal, "an executor needs a collector")
	case cfg.Clock == nil:
		return nil, errs.New(errs.CodeInternal, "an executor needs a clock")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Recorder == nil {
		cfg.Recorder = cfg.Collector
	}
	return &VUs{cfg: cfg}, nil
}

// user is one running VU.
type user struct {
	stop chan struct{}
	done chan struct{}
}

// Run keeps the population at the profile's count until the profile ends or
// arrivalCtx is done. A user asked to stop finishes the iteration it is in; one still
// going when the profile ends has Grace to finish before its context is cut.
func (e *VUs) Run(arrivalCtx, workCtx context.Context) error {
	iterCtx, cutOff := context.WithCancel(workCtx)
	defer cutOff()

	var users []*user
	var wg sync.WaitGroup
	ticker := e.cfg.Clock.NewTicker(vuTick)
	defer ticker.Stop()

	adjust := func(target int) {
		for len(users) < target {
			u := &user{stop: make(chan struct{}), done: make(chan struct{})}
			index := len(users)
			users = append(users, u)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer close(u.done)
				e.loop(iterCtx, u, index)
			}()
		}
		for len(users) > target {
			last := users[len(users)-1]
			close(last.stop)
			users = users[:len(users)-1]
		}
	}

	for {
		elapsed := e.elapsed()
		if elapsed >= e.cfg.Profile.Duration() || arrivalCtx.Err() != nil || workCtx.Err() != nil {
			break
		}
		target := int(math.Round(e.cfg.Profile.RateAt(elapsed)))
		if int64(target) > e.maxVUs.Load() {
			e.maxVUs.Store(int64(target))
		}
		adjust(target)
		select {
		case <-ticker.C():
		case <-arrivalCtx.Done():
		case <-workCtx.Done():
		}
	}
	adjust(0)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	timer := e.cfg.Clock.NewTimer(e.cfg.Grace)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C():
		cutOff()
		<-finished
	case <-workCtx.Done():
		<-finished
	}
	return nil
}

// loop is one user: iterations back to back, with its own random source and a
// session that lasts as long as it runs.
func (e *VUs) loop(ctx context.Context, u *user, index int) {
	rng := rand.New(rand.NewPCG(e.cfg.Seed, uint64(index)+1)) //nolint:gosec // reproducibility, not secrecy
	session := &runner.Session{VU: index, PerVU: e.cfg.PerVU}
	n := e.active.Add(1)
	e.notePeak(n)
	defer e.active.Add(-1)

	for {
		select {
		case <-u.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		now := e.elapsed()
		bucket := e.cfg.Collector.BucketIndexOf(now)
		idx := e.iterations.Add(1)
		e.cfg.Collector.RecordOffered(bucket)
		e.cfg.Collector.ObserveInFlight(bucket, e.active.Load())

		it := runner.Iteration{
			Index: idx, Worker: index,
			Intended: now, Dispatched: now, WorkerStart: now,
			Rand: rng, Session: session,
		}
		if err := e.cfg.Runner.Do(ctx, &it, e.cfg.Recorder); err != nil {
			e.cfg.Logger.Debug("runner could not attempt an operation",
				"runner", e.cfg.Runner.Name(), "vu", index, "err", err)
		}
	}
}

func (e *VUs) elapsed() time.Duration { return e.cfg.Clock.Now().Sub(e.cfg.Start) }

func (e *VUs) notePeak(n int64) {
	for {
		cur := e.peak.Load()
		if n <= cur || e.peak.CompareAndSwap(cur, n) {
			return
		}
	}
}

// Stats reports what the population did. Offered and dispatched are the iterations
// started - a closed model never drops one - and in flight is users running.
func (e *VUs) Stats() Stats {
	n := e.iterations.Load()
	return Stats{
		Offered: n, Dispatched: n,
		PeakInFlight: e.peak.Load(), MaxInFlight: int(e.maxVUs.Load()),
	}
}

// InFlight is the number of users running.
func (e *VUs) InFlight() int64 { return e.active.Load() }

// RecentLag has nothing to report: without a schedule there is nothing to be late
// against.
func (e *VUs) RecentLag() (float64, bool) { return 0, false }
