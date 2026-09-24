package session

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
)

// drawSeed picks a seed for a run nobody seeded. It is recorded in the result, so the
// run can still be reproduced.
func drawSeed() uint64 {
	return rand.Uint64() //nolint:gosec // reproducibility, not secrecy
}

// teeHandler sends every record to two handlers: the caller's stderr logger and the
// run's own run.log.
type teeHandler struct{ a, b slog.Handler }

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.a.Enabled(ctx, l) || t.b.Enabled(ctx, l)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errA, errB error
	if t.a.Enabled(ctx, r.Level) {
		errA = t.a.Handle(ctx, r.Clone())
	}
	if t.b.Enabled(ctx, r.Level) {
		errB = t.b.Handle(ctx, r.Clone())
	}
	return errors.Join(errA, errB)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{t.a.WithAttrs(attrs), t.b.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{t.a.WithGroup(name), t.b.WithGroup(name)}
}
