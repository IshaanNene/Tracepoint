package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Series is everything one sampler produced over a run.
type Series struct {
	Name       string
	Available  bool
	Reason     string
	Interval   time.Duration
	Samples    []Sample
	Statements []map[string]any
}

// Entry is one sampler to run and how often.
type Entry struct {
	Sampler  Sampler
	Interval time.Duration
	// Timing controls whether the loop records sample_ms. The generator's own
	// sampler reads process counters, whose timing says nothing.
	Timing bool
}

// Loop drives every sampler on the shared clock, each on its own goroutine so that a
// datastore which stalls - and a stalling datastore is exactly what we are looking
// for - never delays the others.
type Loop struct {
	clk    clock.Clock
	start  time.Time
	log    *slog.Logger
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	states []*state
}

type state struct {
	entry     Entry
	series    Series
	failures  int
	lastError string
}

// sampleTimeout bounds a single reading. It is generous on purpose: a sample that
// takes three seconds because the server was frozen for three seconds is a finding,
// and cutting it off at the interval would throw that finding away.
const sampleTimeout = 30 * time.Second

// Open connects every sampler that needs it. A sampler that fails to open is recorded
// as unavailable and is not run; the others are unaffected.
func Open(ctx context.Context, clk clock.Clock, log *slog.Logger, entries []Entry) *Loop {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	l := &Loop{clk: clk, log: log}
	for _, e := range entries {
		st := &state{entry: e, series: Series{Name: e.Sampler.Name(), Interval: e.Interval, Available: true}}
		if o, ok := e.Sampler.(Opener); ok {
			if err := o.Open(ctx); err != nil {
				st.series.Available = false
				st.series.Reason = reasonOf(err)
				log.Warn("telemetry sampler unavailable", "sampler", st.series.Name, "reason", st.series.Reason)
			}
		}
		if st.series.Available {
			if lim, ok := e.Sampler.(Limiter); ok {
				st.series.Reason = lim.Limitation()
			}
		}
		l.states = append(l.states, st)
	}
	return l
}

// Start begins sampling, measuring every offset from start - the run's one monotonic
// origin.
func (l *Loop) Start(ctx context.Context, start time.Time) {
	l.start = start
	ctx, l.cancel = context.WithCancel(ctx)
	for _, st := range l.states {
		if !st.series.Available {
			continue
		}
		l.wg.Add(1)
		go func(st *state) {
			defer l.wg.Done()
			l.run(ctx, st)
		}(st)
	}
}

func (l *Loop) run(ctx context.Context, st *state) {
	interval := st.entry.Interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := l.clk.NewTicker(interval)
	defer ticker.Stop()

	// The first reading is taken at once, so counters have a baseline to difference
	// against before the first interval ends.
	l.take(ctx, st)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			l.take(ctx, st)
		}
	}
}

func (l *Loop) take(ctx context.Context, st *state) {
	begun := l.clk.Now()
	at := begun.Sub(l.start)
	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	s, err := st.entry.Sampler.Sample(sctx, at)
	cancel()
	took := l.clk.Now().Sub(begun)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return // stopping, not failing
		}
		st.failures++
		st.lastError = reasonOf(err)
		l.log.Debug("telemetry sample failed", "sampler", st.series.Name, "err", st.lastError)
		return
	}
	if s == nil {
		return // a reading that only primed a baseline
	}
	s["t_ms"] = round3(ms(at))
	if st.entry.Timing {
		s["sample_ms"] = round3(ms(took))
	}
	st.series.Samples = append(st.series.Samples, s)
}

// Stop ends sampling, collects end-of-run snapshots and closes every connection. It
// returns the series in the order the samplers were given.
func (l *Loop) Stop(ctx context.Context) []Series {
	if l.cancel != nil {
		l.cancel()
	}
	l.wg.Wait()

	out := make([]Series, 0, len(l.states))
	for _, st := range l.states {
		if st.series.Available {
			if f, ok := st.entry.Sampler.(Finisher); ok {
				fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				rows, err := f.Finish(fctx)
				cancel()
				if err != nil {
					l.log.Debug("telemetry end-of-run snapshot failed", "sampler", st.series.Name, "err", err)
				} else {
					st.series.Statements = rows
				}
			}
			// A sampler that connected but never managed a single reading told us
			// nothing, and must say why rather than look healthy and empty.
			if len(st.series.Samples) == 0 && st.failures > 0 {
				st.series.Available = false
				st.series.Reason = fmt.Sprintf("every sample failed: %s", st.lastError)
			}
		}
		if c, ok := st.entry.Sampler.(Closer); ok {
			if err := c.Close(); err != nil {
				l.log.Debug("closing a telemetry sampler", "sampler", st.series.Name, "err", err)
			}
		}
		out = append(out, st.series)
	}
	return out
}

// reasonOf turns an error from a datastore into a reason safe to repeat: a coded
// error keeps its own message and hint, anything else is target-supplied text and is
// cleaned and truncated as such.
func reasonOf(err error) string {
	var typed *errs.Error
	if errors.As(err, &typed) {
		if typed.Hint != "" {
			return typed.Message + "; " + typed.Hint
		}
		return typed.Message
	}
	return errs.CleanUntrusted(err.Error())
}
