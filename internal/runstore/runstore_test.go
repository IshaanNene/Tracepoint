package runstore_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func newStore(t *testing.T, clk clock.Clock) *runstore.Store {
	t.Helper()
	s, err := runstore.Open(filepath.Join(t.TempDir(), "runs"), clk)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func create(t *testing.T, s *runstore.Store, id string) *runstore.Run {
	t.Helper()
	r, err := s.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func finish(t *testing.T, r *runstore.Run, status string) {
	t.Helper()
	code := 0
	if err := r.UpdateState(func(st *runstore.State) { st.Status = status; st.ExitCode = &code }); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWritesPendingState(t *testing.T) {
	s := newStore(t, nil)
	r := create(t, s, "20260924T100000Z-aaaaaa")
	st, err := r.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != runstore.StatusPending || st.RunID != r.ID || st.PID != os.Getpid() {
		t.Fatalf("state = %+v", st)
	}
	if st.Artifacts.Result != filepath.Join(r.Dir, "result.json") {
		t.Fatalf("artifacts = %+v", st.Artifacts)
	}
	if _, err := s.Create(r.ID); err == nil {
		t.Fatal("a run directory must never be reused")
	}
}

// A terminal status is final: a late heartbeat must not resurrect a finished run.
func TestTerminalStatusIsFinal(t *testing.T) {
	s := newStore(t, nil)
	r := create(t, s, "20260924T100000Z-aaaaaa")
	finish(t, r, runstore.StatusCompleted)
	if err := r.UpdateState(func(st *runstore.State) { st.Status = runstore.StatusRunning }); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.ReadState(); st.Status != runstore.StatusCompleted {
		t.Fatalf("status = %s", st.Status)
	}
}

func TestGetResolvesIdsPrefixesSuffixesAndPaths(t *testing.T) {
	s := newStore(t, nil)
	create(t, s, "20260924T100000Z-aaaaaa")
	create(t, s, "20260924T110000Z-bbbbbb")

	for ref, want := range map[string]string{
		"20260924T100000Z-aaaaaa": "20260924T100000Z-aaaaaa",
		"20260924T11":             "20260924T110000Z-bbbbbb",
		"bbbbbb":                  "20260924T110000Z-bbbbbb",
		filepath.Join(s.Root(), "20260924T100000Z-aaaaaa"): "20260924T100000Z-aaaaaa",
	} {
		r, err := s.Get(ref)
		if err != nil || r.ID != want {
			t.Errorf("Get(%q) = %v, %v; want %s", ref, r, err, want)
		}
	}
	for _, ref := range []string{"20260924", "zzz", "", "../etc"} {
		_, err := s.Get(ref)
		var typed *errs.Error
		if !errors.As(err, &typed) || typed.Code != errs.CodeRunNotFound {
			t.Errorf("Get(%q): %v, want RUN_NOT_FOUND", ref, err)
		}
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s := newStore(t, nil)
	create(t, s, "20260924T100000Z-aaaaaa")
	create(t, s, "20260924T120000Z-cccccc")
	create(t, s, "20260924T110000Z-bbbbbb")
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].RunID != "20260924T120000Z-cccccc" || all[2].RunID != "20260924T100000Z-aaaaaa" {
		t.Fatalf("list = %+v", all)
	}
}

// A run whose heartbeat went stale and whose process is gone is lost; one whose
// process is alive is merely slow.
func TestLostDetection(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	s := newStore(t, clk)
	r := create(t, s, "20260924T100000Z-aaaaaa")
	if err := r.UpdateState(func(st *runstore.State) {
		st.Status = runstore.StatusRunning
		st.Heartbeat = clk.Now().Format(time.RFC3339Nano)
	}); err != nil {
		t.Fatal(err)
	}
	alive := true
	s.SetProcessCheck(func(int) bool { return alive })

	clk.Advance(runstore.StaleAfter + time.Second)
	if st, _ := r.Status(); st.Status != runstore.StatusRunning {
		t.Fatalf("a live process with a stale heartbeat is %s, want running", st.Status)
	}
	alive = false
	st, _ := r.Status()
	if st.Status != runstore.StatusLost || st.Error == nil {
		t.Fatalf("state = %+v, want lost with an error", st)
	}
	active, _ := s.Active()
	if len(active) != 0 {
		t.Fatalf("a lost run is not active: %+v", active)
	}
}

func TestStopSentinel(t *testing.T) {
	s := newStore(t, nil)
	r := create(t, s, "20260924T100000Z-aaaaaa")
	if r.StopRequested() {
		t.Fatal("no stop was requested")
	}
	if err := r.RequestStop(); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Status(); !r.StopRequested() || !st.StopRequested {
		t.Fatalf("stop not visible: %+v", st)
	}
}

func TestWait(t *testing.T) {
	s := newStore(t, nil)
	r := create(t, s, "20260924T100000Z-aaaaaa")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := r.Wait(ctx, 10*time.Millisecond)
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodeWaitTimeout {
		t.Fatalf("wait on a pending run: %v, want WAIT_TIMEOUT", err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		finish(t, r, runstore.StatusCompleted)
	}()
	st, err := r.Wait(context.Background(), 5*time.Millisecond)
	if err != nil || st.Status != runstore.StatusCompleted {
		t.Fatalf("wait = %+v, %v", st, err)
	}
}

func TestHeartbeatRefreshesState(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	s := newStore(t, clk)
	r := create(t, s, "20260924T100000Z-aaaaaa")
	stop := r.Heartbeat(context.Background(), func() *runstore.Progress {
		return &runstore.Progress{ElapsedMS: 500, TotalMS: 1000, Ratio: 0.5}
	})
	clk.BlockUntil(1)
	clk.Advance(runstore.HeartbeatInterval)
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, _ := r.ReadState()
		if st.Progress != nil && st.Progress.Ratio == 0.5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat never wrote progress: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
}

// gc keeps the newest finished runs and never touches one still going.
func TestGC(t *testing.T) {
	s := newStore(t, nil)
	for _, id := range []string{"20260924T100000Z-aaaaaa", "20260924T110000Z-bbbbbb", "20260924T120000Z-cccccc"} {
		finish(t, create(t, s, id), runstore.StatusCompleted)
	}
	create(t, s, "20260924T090000Z-running") // oldest, but still pending
	removed, err := s.GC(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != "20260924T110000Z-bbbbbb" || removed[1] != "20260924T100000Z-aaaaaa" {
		t.Fatalf("removed = %v", removed)
	}
	all, _ := s.List()
	if len(all) != 2 {
		t.Fatalf("left = %+v", all)
	}
	if _, err := s.GC(-1); err == nil {
		t.Fatal("a negative keep must be refused")
	}
}

func TestAuditAppends(t *testing.T) {
	s := newStore(t, nil)
	for _, ev := range []string{"started", "finished"} {
		if err := s.Audit(runstore.AuditRecord{Event: ev, RunID: "r", Actor: "cli:test"}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(s.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
	}
	if n != 2 {
		t.Fatalf("%d audit lines, want 2", n)
	}
}
