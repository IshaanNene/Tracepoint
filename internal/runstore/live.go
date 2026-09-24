package runstore

import (
	"context"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Wait polls a run until it reaches a terminal status or ctx ends. On timeout it
// returns the latest state together with ctx's error, so the caller can report that
// the run is still going rather than that it failed.
func (r *Run) Wait(ctx context.Context, poll time.Duration) (State, error) {
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	for {
		st, err := r.Status()
		if err != nil {
			return st, err
		}
		if Terminal(st.Status) {
			return st, nil
		}
		t := r.store.clk.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return st, errs.Wrap(errs.CodeWaitTimeout, ctx.Err(), "run %s is still %s", r.ID, st.Status).
				WithHint("the run continues; wait again, or `tracepoint status %s`", r.ID)
		case <-t.C():
		}
	}
}

// Heartbeat refreshes state.json every HeartbeatInterval until the returned function is
// called, so readers can tell a slow run from a dead one. progress, when set, supplies
// how far the run has got.
func (r *Run) Heartbeat(ctx context.Context, progress func() *Progress) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := r.store.clk.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
				now := r.store.clk.Now().UTC().Format(time.RFC3339Nano)
				// A missed beat is retried on the next tick; a run is only declared
				// lost after several are missed and its process is gone.
				if err := r.UpdateState(func(st *State) {
					st.Heartbeat = now
					if progress != nil {
						st.Progress = progress()
					}
				}); err != nil {
					continue
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
