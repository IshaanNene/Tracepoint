package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/engine"
)

// progressEvery is how often the plain progress line is logged, in run time (§5.8).
const progressEvery = 5 * time.Second

// plainProgress logs one line every five seconds of run time, carrying every
// runner's counters, for anywhere the live view is not shown.
type plainProgress struct {
	log     *slog.Logger
	mu      sync.Mutex
	runners []string
	latest  map[string]engine.Progress
	last    time.Duration
}

func newPlainProgress(log *slog.Logger) *plainProgress {
	return &plainProgress{log: log, latest: map[string]engine.Progress{}}
}

// setRunners names the runners in the order the engine reports them, so a line is
// written once the last of them has reported for a round.
func (p *plainProgress) setRunners(names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runners = names
}

func (p *plainProgress) observe(pr engine.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latest[pr.Runner] = pr
	if len(p.runners) == 0 || pr.Runner != p.runners[len(p.runners)-1] || pr.Elapsed-p.last < progressEvery {
		return
	}
	p.last = pr.Elapsed
	args := []any{"elapsed", pr.Elapsed.Round(time.Second).String(), "of", pr.Total.String()}
	for _, name := range p.runners {
		r, ok := p.latest[name]
		if !ok {
			continue
		}
		args = append(args, slog.Group(name,
			"done", r.Done, "errors", r.Errors, "in_flight", r.InFlight, "p99_ms", fmt.Sprintf("%.1f", r.P99MS)))
	}
	p.log.Info("progress", args...)
}

// switchHandler lets the live view take the terminal over from the log for the
// length of a run, and hand it back, without the session having to know.
type switchHandler struct {
	cur *atomic.Pointer[slog.Handler]
	ops []func(slog.Handler) slog.Handler
}

func newSwitchHandler(h slog.Handler) switchHandler {
	cur := &atomic.Pointer[slog.Handler]{}
	cur.Store(&h)
	return switchHandler{cur: cur}
}

func (s switchHandler) set(h slog.Handler) { s.cur.Store(&h) }

func (s switchHandler) resolve() slog.Handler {
	h := *s.cur.Load()
	for _, op := range s.ops {
		h = op(h)
	}
	return h
}

func (s switchHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return s.resolve().Enabled(ctx, l)
}

func (s switchHandler) Handle(ctx context.Context, r slog.Record) error {
	return s.resolve().Handle(ctx, r)
}

func (s switchHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return s.with(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (s switchHandler) WithGroup(name string) slog.Handler {
	return s.with(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

func (s switchHandler) with(op func(slog.Handler) slog.Handler) slog.Handler {
	return switchHandler{cur: s.cur, ops: append(append([]func(slog.Handler) slog.Handler(nil), s.ops...), op)}
}
