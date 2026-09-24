package redisrun_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/runner/redisrun"
)

func TestMain(m *testing.M) {
	// go-redis's pool starts a dial-retry goroutine that sleeps a second between
	// attempts when a server goes away. Closing the client stops it, but only after
	// the current sleep returns, so it can still be parked when the suite ends. It is
	// the library's goroutine and it is bounded; everything of ours is still checked.
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/redis/go-redis/v9/internal/pool.(*ConnPool).tryDial"),
	)
}

func ptr[T any](v T) *T { return &v }

type harness struct {
	runner    *redisrun.Runner
	collector *metrics.Collector
	deps      runner.Deps
	server    *miniredis.Miniredis
}

// newHarness starts an in-process Redis. miniredis speaks the real protocol, so this
// exercises the runner and the client together; a real server is covered by the
// integration suite.
func newHarness(t *testing.T, mutate func(*config.Redis), labels ...string) *harness {
	t.Helper()
	srv := miniredis.RunT(t)

	cfg := &config.Redis{
		Addr:     srv.Addr(),
		Executor: config.Executor{Type: "arrival-rate", Rate: 10, MaxInFlight: 4},
	}
	if mutate != nil {
		mutate(cfg)
	}

	col, err := metrics.NewCollector(metrics.Config{
		Runner: "redis", Kind: metrics.KindStorage, Labels: labels,
		BucketWidth: time.Second, SealDelay: 5 * time.Second, MinSamples: 1,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	deps := runner.Deps{
		RunID: "20260920T120000Z-test01", Collector: col,
		Clock: clock.New(), Start: time.Now(), Seed: 5,
	}
	r, err := redisrun.New(cfg, 3*time.Second, deps)
	if err != nil {
		t.Fatalf("redisrun.New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &harness{runner: r, collector: col, deps: deps, server: srv}
}

func (h *harness) do(t *testing.T, n int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(9, 9))
	for i := range n {
		// Due the moment it is dispatched: a fixed offset would put the intended
		// time in the future whenever the operation is faster than the spacing.
		now := h.deps.Elapsed()
		it := runner.Iteration{
			Index: int64(i), Intended: now, Dispatched: now,
			WorkerStart: now, Rand: rng,
		}
		if err := h.runner.Do(context.Background(), &it, h.collector); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	h.collector.Finish(time.Hour)
}

func TestReadsAndWrites(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{
			{Name: "get", Cmd: []string{"GET", "k"}},
			{Name: "set", Cmd: []string{"SET", "k", "v"}},
		}
	}, "get", "set")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 40)

	s := h.collector.Snapshot().Summary
	if s.N != 40 || s.OK != 40 {
		t.Errorf("n=%d ok=%d, want 40/40 (errors %v)", s.N, s.OK, s.Errors)
	}
	reads, writes := h.runner.Counts()
	if reads == 0 || writes == 0 {
		t.Errorf("reads=%d writes=%d, want both; the classifier should have split them", reads, writes)
	}
	if reads+writes != 40 {
		t.Errorf("reads+writes = %d, want 40", reads+writes)
	}
}

// doConcurrently drives the runner from several goroutines at once, the way the
// executor's workers do. Run under -race it is what catches shared state that a
// sequential harness never exercises.
func (h *harness) doConcurrently(t *testing.T, workers, each int) {
	t.Helper()
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(worker), 1)) //nolint:gosec // a test
			for i := range each {
				now := h.deps.Elapsed()
				it := runner.Iteration{
					Index: int64(worker*each + i), Worker: worker, Intended: now, Dispatched: now,
					WorkerStart: now, Rand: rng,
				}
				if err := h.runner.Do(context.Background(), &it, h.collector); err != nil {
					t.Errorf("Do: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	h.collector.Finish(time.Hour)
}

// The known-answer suite found the read and write tallies racing between workers.
func TestConcurrentWorkersCountExactly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{
			{Name: "get", Cmd: []string{"GET", "k"}},
			{Name: "set", Cmd: []string{"SET", "k", "v"}},
		}
	}, "get", "set")
	h.doConcurrently(t, 8, 25)
	if reads, writes := h.runner.Counts(); reads+writes != 200 {
		t.Fatalf("reads+writes = %d, want 200", reads+writes)
	}
}

// A GET for a key that is not there is an ordinary answer, not a failure. Counting it
// as an error would make a cache with a low hit rate look like a broken cache.
func TestMissingKeyIsNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{Name: "get-absent", Cmd: []string{"GET", "never-set"}}}
	}, "get-absent")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 10)

	s := h.collector.Snapshot().Summary
	if s.OK != 10 {
		t.Errorf("ok = %d, want 10; a cache miss is an answer (errors %v)", s.OK, s.Errors)
	}
}

func TestGeneratedArguments(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{
			Name: "get-session", Type: "read",
			Cmd: []string{"GET", "session:{{randInt 1 50}}"},
		}}
	}, "get-session")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 30)
	if s := h.collector.Snapshot().Summary; s.OK != 30 {
		t.Errorf("ok = %d, want 30 (errors %v)", s.OK, s.Errors)
	}
}

func TestPipelineIsTimedAsOneUnit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{
			Name: "batch", Type: "write",
			Pipeline: [][]string{
				{"SET", "a", "1"},
				{"SET", "b", "2"},
				{"GET", "a"},
			},
		}}
	}, "batch")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 10)

	s := h.collector.Snapshot().Summary
	// Ten iterations of a three-command pipeline are ten operations, not thirty: the
	// pipeline is one round trip and one unit of user-visible work.
	if s.N != 10 || s.OK != 10 {
		t.Errorf("n=%d ok=%d, want 10/10 (errors %v)", s.N, s.OK, s.Errors)
	}
	if got, err := h.server.Get("a"); err != nil || got != "1" {
		t.Errorf("the pipeline did not run: a=%q err=%v", got, err)
	}
}

// The server answering "no" says something about the command, not about the server's
// health, so it must not trip the abort guard.
func TestServerRefusalIsAQueryError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{
			Name: "wrong-type", Type: "read",
			// INCR on a key holding a string that is not a number is refused.
			Cmd: []string{"INCR", "notanumber"},
		}}
	}, "wrong-type")
	if err := h.server.Set("notanumber", "hello"); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 5)

	s := h.collector.Snapshot().Summary
	if s.Errors["query_error"] != 5 {
		t.Errorf("errors = %v, want five query_error", s.Errors)
	}
	if metrics.ClassQueryError.CountsTowardAbortGuard() {
		t.Error("a refusal from a healthy server must not trip the abort guard")
	}
}

func TestConnectionFailureIsClassified(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{Name: "get", Cmd: []string{"GET", "k"}}}
	}, "get")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.server.Close() // the server goes away mid-run, as a real one might
	h.do(t, 3)

	s := h.collector.Snapshot().Summary
	if s.Errors["connection"] == 0 && s.Errors["timeout"] == 0 {
		t.Errorf("errors = %v, want a connection failure after the server went away", s.Errors)
	}
}

func TestPreflightFailsAgainstADeadServer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{Name: "get", Cmd: []string{"GET", "k"}}}
	}, "get")
	h.server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := h.runner.Prepare(ctx)
	if err == nil {
		t.Fatal("preflight succeeded against a server that is not running")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodePreflightRedisPing {
		t.Errorf("code = %v, want %s", err, errs.CodePreflightRedisPing)
	}
}

func TestWeightedCommandSelection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{
			{Name: "hot", Weight: ptr(4.0), Cmd: []string{"GET", "a"}},
			{Name: "cold", Weight: ptr(1.0), Cmd: []string{"GET", "b"}},
		}
	}, "hot", "cold")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 400)

	labels := h.collector.Snapshot().LabelSummaries
	hot, cold := labels["hot"].N, labels["cold"].N
	if hot+cold != 400 {
		t.Fatalf("hot=%d cold=%d, want 400", hot, cold)
	}
	if ratio := float64(hot) / 400; ratio < 0.7 || ratio > 0.9 {
		t.Errorf("the 4:1 weighting produced %d:%d (%.2f), want about 0.8", hot, cold, ratio)
	}
}

// A declared type overrides the classifier, because the author knows what the command
// does and the table is inferring.
func TestDeclaredTypeOverridesTheClassifier(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{Name: "eval-read", Type: "read", Cmd: []string{"GET", "k"}}}
	}, "eval-read")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 5)
	reads, writes := h.runner.Counts()
	if reads != 5 || writes != 0 {
		t.Errorf("reads=%d writes=%d, want 5/0", reads, writes)
	}
}

func TestMetadataAndPoolStats(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{Name: "ping", Cmd: []string{"PING"}}}
	}, "ping")
	if h.runner.Name() != "redis" {
		t.Errorf("Name() = %q", h.runner.Name())
	}
	if h.runner.Kind() != metrics.KindStorage {
		t.Errorf("Kind() = %s, want storage", h.runner.Kind())
	}
	if got := h.runner.Labels(); len(got) != 1 || got[0] != "ping" {
		t.Errorf("Labels() = %v", got)
	}
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 3)
	if stats := h.runner.PoolStats(); stats == nil {
		t.Error("PoolStats() returned nothing; pool waits are how a client-side queue is spotted")
	}
}

// A template that cannot be rendered must fail the operation rather than sending a
// literal token, which would be a command against a key that does not exist (§4).
func TestUnresolvableTemplateFailsTheOperation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{Name: "needs-var", Cmd: []string{"GET", "session:{{no_such_var}}"}}}
	}, "needs-var")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 3)

	s := h.collector.Snapshot().Summary
	if s.Errors["extract_failed"] != 3 {
		t.Errorf("errors = %v, want three extract_failed", s.Errors)
	}
	// Nothing must have reached the server with a literal token in it.
	for _, key := range h.server.Keys() {
		if strings.Contains(key, "{{") {
			t.Errorf("a literal template token reached the server as key %q", key)
		}
	}
}

func TestTimeoutIsClassified(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Redis) {
		c.Commands = []config.Command{{
			Name: "slow", Cmd: []string{"GET", "k"},
			Timeout: ptr(config.Duration(time.Nanosecond)),
		}}
	}, "slow")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 3)

	s := h.collector.Snapshot().Summary
	if s.Errors["timeout"] == 0 && s.Errors["canceled"] == 0 {
		t.Errorf("errors = %v, want a timeout with a one-nanosecond budget", s.Errors)
	}
}
