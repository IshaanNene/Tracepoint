// Package engine runs a configuration end to end.
//
// It owns the one thing no other package can: the single monotonic start instant that
// every runner and sampler measures from. Everything else here is wiring - build the
// collectors and runners, preflight them, drive the executors, seal buckets on a
// ticker, drain gracefully, and hand the recorded data to the result builder.
//
// The engine performs no analysis. Analysis is a pure function of the result document
// (docs/adr/004), so it happens after this package is done.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/executor"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/runner/httprun"
	"github.com/IshaanNene/Tracepoint/internal/schedule"
)

// sealInterval is how often sealed buckets are swept up. Frequent enough that memory
// is released promptly, rare enough to cost nothing.
const sealInterval = 500 * time.Millisecond

// Options configure a run. Everything the engine depends on is injected, so a run is
// reproducible and testable without touching the process environment.
type Options struct {
	Config     *config.Config
	SourcePath string
	Overrides  []string
	Clock      clock.Clock
	Logger     *slog.Logger
	RunID      string
	// Seed overrides the configured seed. When neither is set one is drawn and
	// recorded, so any run can be reproduced from its own result.
	Seed *uint64
	// Progress, when set, is called about once a second with a live summary.
	Progress func(Progress)
}

// Progress is a live view of a run, for the terminal or an event stream.
type Progress struct {
	Elapsed  time.Duration
	Total    time.Duration
	Runner   string
	Offered  int64
	Done     int64
	Errors   int64
	InFlight int64
	P99MS    float64
}

// Engine runs one configuration.
type Engine struct {
	opts  Options
	clk   clock.Clock
	log   *slog.Logger
	seed  uint64
	start time.Time

	runners []*boundRunner
}

// boundRunner is a runner with everything it needs to be driven.
type boundRunner struct {
	name      string
	runner    runner.Runner
	collector *metrics.Collector
	exec      *executor.ArrivalRate
	stages    []result.Stage
	targets   []result.Target
}

// New prepares an engine. It does not touch the network; Run does.
func New(opts Options) (*Engine, error) {
	if opts.Config == nil {
		return nil, errs.New(errs.CodeInternal, "the engine needs a configuration")
	}
	if opts.Clock == nil {
		opts.Clock = clock.New()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	e := &Engine{opts: opts, clk: opts.Clock, log: opts.Logger}

	switch {
	case opts.Seed != nil:
		e.seed = *opts.Seed
	case opts.Config.Run.Seed != nil:
		e.seed = *opts.Config.Run.Seed
	default:
		// Draw one and record it, so a run nobody seeded can still be reproduced.
		e.seed = rand.Uint64() //nolint:gosec // reproducibility, not secrecy
	}
	if opts.RunID == "" {
		e.opts.RunID = NewRunID(e.clk.Now(), e.seed)
	}
	return e, nil
}

// NewRunID builds a run identifier: a UTC timestamp and a short suffix, which sorts
// chronologically and is also the run directory's name.
func NewRunID(now time.Time, seed uint64) string {
	suffix := rand.New(rand.NewPCG(seed, uint64(now.UnixNano()))).Uint64() //nolint:gosec // an identifier, not a secret
	return fmt.Sprintf("%s-%06x", now.UTC().Format("20060102T150405Z"), suffix&0xffffff)
}

// RunID is the identifier this run will be recorded under.
func (e *Engine) RunID() string { return e.opts.RunID }

// Seed is the seed actually in use, whether configured or drawn.
func (e *Engine) Seed() uint64 { return e.seed }

// Run performs the whole run and returns the result document.
//
// Cancelling ctx is a graceful stop: arrivals cease, in-flight work is given the
// configured grace period to finish, and a partial result is produced and marked
// interrupted. A run that is stopped still has something to say.
func (e *Engine) Run(ctx context.Context) (*result.Result, error) {
	cfg := e.opts.Config

	if err := e.build(); err != nil {
		return nil, err
	}
	defer e.closeRunners()

	// Preflight before the clock starts, so its cost is not charged to the run and a
	// misconfigured target is reported before a single measurement is taken.
	if err := e.preflight(ctx); err != nil {
		return nil, err
	}

	e.start = e.clk.Now()
	startedAt := e.start
	for _, br := range e.runners {
		br.exec = nil // rebuilt below now that the start instant exists
	}
	if err := e.buildExecutors(); err != nil {
		return nil, err
	}

	// Arrivals follow the caller: cancelling ctx is a graceful stop and no further load
	// is offered.
	arrivalCtx, stopArrivals := context.WithCancel(ctx)
	defer stopArrivals()

	// Work deliberately does not inherit ctx. That is the whole of a graceful drain:
	// when the caller cancels, in-flight operations must be allowed to finish within
	// the grace period rather than being cut off with it. Deriving this from ctx would
	// abandon them and record a burst of cancellations as the run's final act.
	workCtx, cutOffWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cutOffWork()

	var status atomic.Value
	status.Store(result.StatusCompleted)
	var interrupted atomic.Value
	interrupted.Store("")

	watch := context.AfterFunc(ctx, func() {
		status.Store(result.StatusInterrupted)
		interrupted.Store("stopped before the configured duration elapsed")
	})
	defer watch()

	sweeper := e.startSweeper(workCtx)

	var wg sync.WaitGroup
	errCh := make(chan error, len(e.runners))
	for _, br := range e.runners {
		wg.Add(1)
		go func(br *boundRunner) {
			defer wg.Done()
			if err := br.exec.Run(arrivalCtx, workCtx); err != nil {
				errCh <- fmt.Errorf("runner %s: %w", br.name, err)
			}
		}(br)
	}

	// Arrivals end when the profile is exhausted; drain bounds how long in-flight work
	// may continue past that before it is cut off.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	grace := cfg.Run.Grace.D()
	select {
	case <-done:
	case <-e.after(grace + cfg.Run.Duration.D() + time.Minute):
		// A safety net, not the normal path: the executors bound themselves.
		cutOffWork()
		<-done
	}

	cutOffWork()
	sweeper()
	elapsed := e.clk.Now().Sub(e.start)

	close(errCh)
	for err := range errCh {
		if err != nil {
			return nil, err
		}
	}

	for _, br := range e.runners {
		br.collector.Finish(elapsed)
	}

	return e.buildResult(startedAt, elapsed, loadString(&status), loadString(&interrupted))
}

// loadString reads a string written by the signal watcher on another goroutine.
func loadString(v *atomic.Value) string {
	s, ok := v.Load().(string)
	if !ok {
		return ""
	}
	return s
}

func (e *Engine) after(d time.Duration) <-chan time.Time {
	t := e.clk.NewTimer(d)
	return t.C()
}

// build creates the collectors and runners. Executors are built later, once the run
// start instant exists.
func (e *Engine) build() error {
	cfg := e.opts.Config
	for _, name := range cfg.Runners() {
		if name != httprun.Name {
			return errs.New(errs.CodeConfigInvalidValue,
				"the %s runner is not in this build yet", name).
				WithPath("/"+name).
				WithHint("SQL and Redis runners land in phase 2; available now: %v", runner.Registered())
		}
		col, err := metrics.NewCollector(metrics.Config{
			Runner:        name,
			Kind:          metrics.Kind(config.Kind(name)),
			Labels:        cfg.Labels(name),
			BucketWidth:   cfg.Run.Bucket.D(),
			Warmup:        cfg.Run.Warmup.D(),
			SealDelay:     cfg.Run.Timeout.D(),
			MinSamples:    cfg.Run.MinSamples,
			TrackStatuses: name == httprun.Name,
		})
		if err != nil {
			return err
		}
		deps := runner.Deps{
			RunID: e.opts.RunID, Collector: col, Clock: e.clk,
			Seed: e.seed, Logger: e.log,
		}
		// Start is filled in when the run actually begins; runners read it through
		// Deps, so the value has to be set before any operation is performed.
		r, err := httprun.New(cfg.HTTP, cfg.Run.Duration.D(), cfg.Run.Timeout.D(), deps)
		if err != nil {
			return err
		}
		e.runners = append(e.runners, &boundRunner{name: name, runner: r, collector: col})
	}
	if len(e.runners) == 0 {
		return errs.New(errs.CodeConfigNoRunner, "no runner is configured")
	}
	return nil
}

// buildExecutors compiles each runner's load profile now that the start instant is
// known. Every runner shares that instant, which is what puts all three tiers on one
// timeline.
func (e *Engine) buildExecutors() error {
	cfg := e.opts.Config
	for _, br := range e.runners {
		ex := cfg.Executor(br.name)
		if ex == nil {
			return errs.New(errs.CodeInternal, "runner %s has no executor", br.name)
		}
		stages := make([]schedule.Stage, 0, len(ex.Stages))
		for _, s := range ex.Stages {
			stages = append(stages, schedule.Stage{Duration: s.Duration.D(), Target: s.Target})
			br.stages = append(br.stages, result.Stage{DurationMS: float64(s.Duration.D()) / float64(time.Millisecond), Target: s.Target})
		}
		profile, err := schedule.NewProfile(ex.StartRate(), stages)
		if err != nil {
			return err
		}
		// Each runner gets its own source, derived from the run seed and the runner's
		// position, so adding a runner does not change another's sequence.
		rng := rand.New(rand.NewPCG(e.seed, uint64(len(br.name)))) //nolint:gosec // reproducibility, not secrecy
		sched, err := schedule.NewSchedule(profile, schedule.Arrival(cfg.Run.Arrival), rng)
		if err != nil {
			return err
		}

		queue := executor.DefaultQueueDepth
		if ex.QueueDepth != nil {
			queue = *ex.QueueDepth
		}
		exec, err := executor.NewArrivalRate(executor.Config{
			Runner: br.runner, Schedule: sched, Collector: br.collector,
			Clock: e.clk, Start: e.start,
			MaxInFlight: ex.MaxInFlight, QueueDepth: queue,
			Seed: e.seed, Logger: e.log,
		})
		if err != nil {
			return err
		}
		br.exec = exec

		// Runners need the start instant to convert their own stamps into offsets.
		if h, ok := br.runner.(*httprun.Runner); ok {
			h.SetStart(e.start)
		}
	}
	return nil
}

// preflight checks every target before any load starts.
func (e *Engine) preflight(ctx context.Context) error {
	for _, br := range e.runners {
		if err := br.runner.Prepare(ctx); err != nil {
			return err
		}
		// Targets are resolved here, while a context exists, and the classification is
		// kept for the result: it drives the same-host caveat and, from phase 2, the
		// policy decision.
		if h, ok := br.runner.(*httprun.Runner); ok {
			br.targets = resolveTargets(ctx, h.Targets())
		}
	}
	return nil
}

// startSweeper seals buckets on a ticker and reports progress. It returns a function
// that stops it and waits for it to finish.
func (e *Engine) startSweeper(ctx context.Context) func() {
	ticker := e.clk.NewTicker(sealInterval)
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer ticker.Stop()
		var lastProgress time.Duration
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C():
				elapsed := e.clk.Now().Sub(e.start)
				for _, br := range e.runners {
					br.collector.SealThrough(elapsed)
				}
				if e.opts.Progress != nil && elapsed-lastProgress >= time.Second {
					lastProgress = elapsed
					e.reportProgress(elapsed)
				}
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

func (e *Engine) reportProgress(elapsed time.Duration) {
	for _, br := range e.runners {
		st := br.exec.Stats()
		snap := br.collector.Snapshot()
		e.opts.Progress(Progress{
			Elapsed: elapsed, Total: e.opts.Config.Run.Duration.D(),
			Runner: br.name, Offered: st.Offered, Done: snap.Summary.N,
			Errors: snap.Summary.ErrorsTotal, InFlight: br.exec.InFlight(),
			P99MS: snap.Summary.Response.P99,
		})
	}
}

func (e *Engine) closeRunners() {
	for _, br := range e.runners {
		if err := br.runner.Close(); err != nil {
			e.log.Debug("closing a runner", "runner", br.name, "err", err)
		}
	}
}

func (e *Engine) buildResult(startedAt time.Time, elapsed time.Duration, status, interrupted string) (*result.Result, error) {
	in := result.BuildInput{
		RunID: e.opts.RunID, Config: e.opts.Config, SourcePath: e.opts.SourcePath,
		Overrides: e.opts.Overrides, Seed: e.seed,
		StartedAt: startedAt, FinishedAt: e.clk.Now(), Elapsed: elapsed,
		Status: status, Interrupted: interrupted,
	}
	for _, br := range e.runners {
		ri := result.RunnerInput{
			Snapshot: br.collector.Snapshot(),
			Stats:    br.exec.Stats(),
			Stages:   br.stages,
		}
		ri.Targets = br.targets
		if h, ok := br.runner.(*httprun.Runner); ok {
			phases := h.PhaseBreakdown()
			ri.HTTP = &result.HTTPDetail{
				StatusHistogram: ri.Snapshot.StatusHistogram,
				ConnReuseRatio:  h.ConnectionReuse(),
				InsecureTLS:     h.InsecureTLS(),
				PhasesMS: map[string]result.Quantiles{
					"dns": q(phases.DNS), "connect": q(phases.Connect), "tls": q(phases.TLS),
					"conn_wait": q(phases.ConnWait), "ttfb": q(phases.TTFB), "transfer": q(phases.Transfer),
				},
			}
		}
		if ri.Snapshot.LabelTimelineDropped {
			in.Warnings = append(in.Warnings, result.Finding{
				Code: "LABEL_TIMELINE_DROPPED", Severity: result.SeverityInfo,
				Message: fmt.Sprintf("per-label timelines for %s exceeded the retention budget and were dropped", br.name),
				Fix:     "charts lose per-label detail; no analysis is affected",
			})
		}
		if ri.Snapshot.LabelOverflow {
			in.Warnings = append(in.Warnings, result.Finding{
				Code: "LABEL_OVERFLOW", Severity: result.SeverityInfo,
				Message: fmt.Sprintf("%s reported more than %d labels; the rest were collected under \"other\"", br.name, metrics.MaxLabels),
			})
		}
		if ri.Snapshot.Late > 0 {
			in.Warnings = append(in.Warnings, result.Finding{
				Code: "INSUFFICIENT_SAMPLES", Severity: result.SeverityInfo,
				Message: fmt.Sprintf("%d %s operations finished after their bucket had sealed and were counted against the run total only", ri.Snapshot.Late, br.name),
				Fix:     "raise run.timeout if operations routinely take longer than it allows",
			})
		}
		in.Runners = append(in.Runners, ri)
	}
	return result.Build(in)
}

func q(v metrics.Quantiles) result.Quantiles {
	return result.Quantiles{P50: v.P50, P90: v.P90, P95: v.P95, P99: v.P99, P999: v.P999, Max: v.Max, Mean: v.Mean}
}

// resolveTargets classifies each host, which drives the same-host caveat and, from
// phase 2, the policy decision.
func resolveTargets(ctx context.Context, hosts []string) []result.Target {
	var resolver net.Resolver
	out := make([]result.Target, 0, len(hosts))
	for _, h := range hosts {
		t := result.Target{Host: h}
		addrs, err := resolver.LookupHost(ctx, h)
		if err != nil {
			out = append(out, t)
			continue
		}
		t.Addrs = addrs
		t.Scope = classifyScope(addrs)
		out = append(out, t)
	}
	return out
}

// classifyScope reports the widest scope among a host's addresses. Widest, because a
// host that resolves to both a private and a public address is reachable publicly, and
// the safety decision must be made on that.
func classifyScope(addrs []string) string {
	scope := result.ScopeLoopback
	rank := map[string]int{
		result.ScopeLoopback: 0, result.ScopeLinkLocal: 1,
		result.ScopePrivate: 2, result.ScopePublic: 3,
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		var s string
		switch {
		case ip.IsLoopback():
			s = result.ScopeLoopback
		case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
			s = result.ScopeLinkLocal
		case ip.IsPrivate():
			s = result.ScopePrivate
		default:
			s = result.ScopePublic
		}
		if rank[s] > rank[scope] {
			scope = s
		}
	}
	return scope
}
