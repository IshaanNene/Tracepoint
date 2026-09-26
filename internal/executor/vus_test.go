package executor_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/executor"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/schedule"
)

// sessionRunner records which session each user's iterations carried.
type sessionRunner struct {
	fakeRunner
	mu       sync.Mutex
	sessions map[int]map[*runner.Session]bool
	vuOf     map[*runner.Session]int
}

func (s *sessionRunner) Do(ctx context.Context, it *runner.Iteration, rec metrics.Recorder) error {
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions, s.vuOf = map[int]map[*runner.Session]bool{}, map[*runner.Session]int{}
	}
	if s.sessions[it.Worker] == nil {
		s.sessions[it.Worker] = map[*runner.Session]bool{}
	}
	s.sessions[it.Worker][it.Session] = true
	s.vuOf[it.Session] = it.Session.VU
	s.mu.Unlock()
	return s.fakeRunner.Do(ctx, it, rec)
}

func vuProfile(t *testing.T, start float64, stages ...schedule.Stage) *schedule.Profile {
	t.Helper()
	p, err := schedule.NewProfile(start, stages)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runVUs(t *testing.T, r runner.Runner, p *schedule.Profile, grace time.Duration) (*executor.VUs, *metrics.Collector, time.Duration) {
	t.Helper()
	clk := clock.New()
	start := clk.Now()
	col := newCollector(t)
	r.SetStart(start)
	e, err := executor.NewVUs(executor.VUsConfig{Runner: r, Profile: p, Collector: col, Clock: clk, Start: start, Grace: grace, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	took := clk.Now().Sub(start)
	col.Finish(time.Hour)
	return e, col, took
}

// A constant population loops as fast as the target answers: three users against a
// 20ms service offer about 150 iterations a second, and never more than three at once.
func TestVUsHoldAPopulation(t *testing.T) {
	r := &fakeRunner{clk: clock.New(), service: 20 * time.Millisecond}
	e, col, _ := runVUs(t, r, vuProfile(t, 3, schedule.Stage{Duration: 500 * time.Millisecond, Target: 3}), time.Second)
	st := e.Stats()
	if st.PeakInFlight != 3 || st.MaxInFlight != 3 {
		t.Fatalf("peak %d, max %d, want 3 and 3", st.PeakInFlight, st.MaxInFlight)
	}
	if st.Offered < 45 || st.Offered > 80 {
		t.Fatalf("%d iterations from 3 users over 500ms at 20ms each, want about 75", st.Offered)
	}
	if st.Dropped != 0 || st.Offered != st.Dispatched {
		t.Fatalf("a closed model never drops: %+v", st)
	}
	if n := col.Snapshot().Summary.N; n != st.Offered {
		t.Fatalf("%d outcomes for %d iterations", n, st.Offered)
	}
}

// A population ramps up and back down with its stages; a user leaving finishes the
// iteration it is in.
func TestVUsFollowTheirStages(t *testing.T) {
	r := &fakeRunner{clk: clock.New(), service: 10 * time.Millisecond}
	e, _, took := runVUs(t, r, vuProfile(t, 0,
		schedule.Stage{Duration: 300 * time.Millisecond, Target: 6},
		schedule.Stage{Duration: 300 * time.Millisecond, Target: 0},
	), time.Second)
	if st := e.Stats(); st.PeakInFlight < 5 || st.MaxInFlight < 5 {
		t.Fatalf("ramp to 6 peaked at %d users (target max %d)", st.PeakInFlight, st.MaxInFlight)
	}
	if took > 900*time.Millisecond {
		t.Fatalf("a 600ms profile took %s", took)
	}
	if e.InFlight() != 0 {
		t.Fatal("users still running after Run returned")
	}
	if r.started.Load() != r.finished.Load() {
		t.Fatalf("%d started, %d finished: a stopping user must finish its iteration", r.started.Load(), r.finished.Load())
	}
}

// Each user keeps one session for as long as it runs, and users do not share one.
func TestVUsKeepASessionPerUser(t *testing.T) {
	r := &sessionRunner{fakeRunner: fakeRunner{clk: clock.New(), service: 10 * time.Millisecond}}
	runVUs(t, r, vuProfile(t, 2, schedule.Stage{Duration: 200 * time.Millisecond, Target: 2}), time.Second)
	if len(r.sessions) != 2 {
		t.Fatalf("%d users ran, want 2", len(r.sessions))
	}
	for vu, set := range r.sessions {
		if len(set) != 1 {
			t.Fatalf("user %d saw %d sessions, want one for the run", vu, len(set))
		}
		for s := range set {
			if r.vuOf[s] != vu {
				t.Fatalf("session numbered %d ran on user %d", r.vuOf[s], vu)
			}
		}
	}
}

// When the profile ends, users still mid-iteration get the grace period and are then
// cut off, recorded as canceled - the run never hangs on a stuck target.
func TestVUsCutOffAfterGrace(t *testing.T) {
	r := &fakeRunner{clk: clock.New(), gate: make(chan struct{})}
	_, col, took := runVUs(t, r, vuProfile(t, 2, schedule.Stage{Duration: 100 * time.Millisecond, Target: 2}), 150*time.Millisecond)
	if took > 600*time.Millisecond {
		t.Fatalf("a 100ms profile with 150ms grace took %s", took)
	}
	if n := col.Snapshot().Summary.Errors["canceled"]; n != 2 {
		t.Fatalf("canceled = %d, want the two stuck users", n)
	}
}

// Cancelling arrivals stops the population at once, gracefully.
func TestVUsStopOnCancel(t *testing.T) {
	r := &fakeRunner{clk: clock.New(), service: 5 * time.Millisecond}
	clk := clock.New()
	start := clk.Now()
	col := newCollector(t)
	e, err := executor.NewVUs(executor.VUsConfig{Runner: r, Profile: vuProfile(t, 2, schedule.Stage{Duration: time.Hour, Target: 2}),
		Collector: col, Clock: clk, Start: start, Grace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := e.Run(ctx, context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := clk.Now().Sub(start); took > 500*time.Millisecond {
		t.Fatalf("cancelled after 150ms, returned after %s", took)
	}
}
