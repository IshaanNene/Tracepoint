package telemetry

import (
	"context"
	"errors"
	"math"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// fake is a scriptable sampler.
type fake struct {
	name     string
	openErr  error
	sampleFn func(ctx context.Context) (Sample, error)
	finished atomic.Bool
	closed   atomic.Bool
	calls    atomic.Int64
}

func (f *fake) Name() string               { return f.name }
func (f *fake) Open(context.Context) error { return f.openErr }
func (f *fake) Close() error               { f.closed.Store(true); return nil }
func (f *fake) Limitation() string         { return "" }
func (f *fake) Finish(context.Context) ([]map[string]any, error) {
	f.finished.Store(true)
	return []map[string]any{{"query": "SELECT 1"}}, nil
}
func (f *fake) Sample(ctx context.Context, _ time.Duration) (Sample, error) {
	f.calls.Add(1)
	if f.sampleFn != nil {
		return f.sampleFn(ctx)
	}
	return Sample{"v": 1.0}, nil
}

func runLoop(t *testing.T, d time.Duration, entries ...Entry) []Series {
	t.Helper()
	ctx := context.Background()
	l := Open(ctx, clock.New(), nil, entries)
	l.Start(ctx, time.Now())
	time.Sleep(d)
	return l.Stop(ctx)
}

func TestLoopSamplesOnTheSharedClock(t *testing.T) {
	f := &fake{name: "a"}
	out := runLoop(t, 80*time.Millisecond, Entry{Sampler: f, Interval: 10 * time.Millisecond, Timing: true})
	s := out[0]
	if !s.Available || len(s.Samples) < 3 {
		t.Fatalf("series = %+v, want several samples", s)
	}
	prev := -1.0
	for _, smp := range s.Samples {
		tm, ok := smp["t_ms"].(float64)
		if !ok || tm < prev {
			t.Fatalf("t_ms not monotonic: %v", s.Samples)
		}
		prev = tm
		if _, ok := smp["sample_ms"]; !ok {
			t.Fatalf("sample_ms missing: %v", smp)
		}
	}
	if !f.finished.Load() || !f.closed.Load() || len(s.Statements) != 1 {
		t.Fatalf("finish/close not called: finished=%v closed=%v", f.finished.Load(), f.closed.Load())
	}
}

// A datastore that stalls stalls only its own sampler.
func TestLoopIsolatesAStalledSampler(t *testing.T) {
	stuck := &fake{name: "stuck", sampleFn: func(ctx context.Context) (Sample, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	healthy := &fake{name: "healthy"}
	out := runLoop(t, 60*time.Millisecond,
		Entry{Sampler: stuck, Interval: 10 * time.Millisecond},
		Entry{Sampler: healthy, Interval: 10 * time.Millisecond})
	if len(out[1].Samples) < 3 {
		t.Fatalf("the healthy sampler was held up: %d samples", len(out[1].Samples))
	}
	// Stopping is not failing: a sample cut off by shutdown is not an error.
	if !out[0].Available {
		t.Fatalf("stuck sampler marked unavailable on shutdown: %+v", out[0])
	}
}

func TestLoopFailsSoft(t *testing.T) {
	refused := &fake{name: "refused", openErr: errors.New("permission denied for pg_stat_activity\x1b[31m")}
	broken := &fake{name: "broken", sampleFn: func(context.Context) (Sample, error) {
		return nil, errors.New("relation does not exist")
	}}
	out := runLoop(t, 40*time.Millisecond,
		Entry{Sampler: refused, Interval: 10 * time.Millisecond},
		Entry{Sampler: broken, Interval: 10 * time.Millisecond})

	if out[0].Available || out[0].Reason != "permission denied for pg_stat_activity" || refused.calls.Load() != 0 {
		t.Fatalf("refused: %+v (calls %d)", out[0], refused.calls.Load())
	}
	if out[1].Available || !strings.Contains(out[1].Reason, "every sample failed: relation does not exist") {
		t.Fatalf("broken: %+v", out[1])
	}
}

// A nil sample primes a baseline and records nothing.
func TestLoopSkipsPrimingSamples(t *testing.T) {
	var n atomic.Int64
	f := &fake{name: "p", sampleFn: func(context.Context) (Sample, error) {
		if n.Add(1) == 1 {
			return nil, nil
		}
		return Sample{"v": 1.0}, nil
	}}
	out := runLoop(t, 40*time.Millisecond, Entry{Sampler: f, Interval: 10 * time.Millisecond})
	if int64(len(out[0].Samples)) != f.calls.Load()-1 {
		t.Fatalf("%d samples from %d calls", len(out[0].Samples), f.calls.Load())
	}
}

func TestCounters(t *testing.T) {
	var c counters
	t0 := time.Unix(0, 0)
	if _, _, ok := c.step(t0, map[string]float64{"a": 10}); ok {
		t.Fatalf("the first reading only primes")
	}
	d, el, ok := c.step(t0.Add(2*time.Second), map[string]float64{"a": 25, "b": 3})
	if !ok || d["a"] != 15 || el != 2*time.Second {
		t.Fatalf("deltas = %v over %v", d, el)
	}
	if _, has := d["b"]; has {
		t.Fatalf("a counter with no previous reading has no delta")
	}
	// A reset counter is dropped, not reported as negative.
	d, _, _ = c.step(t0.Add(3*time.Second), map[string]float64{"a": 4, "b": 5})
	if _, has := d["a"]; has || d["b"] != 2 {
		t.Fatalf("deltas after reset = %v", d)
	}
}

func TestParseInfo(t *testing.T) {
	raw := "# Stats\r\ntotal_commands_processed:120\r\nkeyspace_hits:7\r\nrole:master\r\n\r\n# Clients\r\nblocked_clients:2\r\n"
	got := parseInfo(raw)
	if got["total_commands_processed"] != 120 || got["keyspace_hits"] != 7 || got["blocked_clients"] != 2 {
		t.Fatalf("parsed %v", got)
	}
	if _, has := got["role"]; has {
		t.Fatalf("non-numeric fields are skipped: %v", got)
	}
}

func TestHistP99(t *testing.T) {
	buckets := []float64{0, 0.001, 0.002, 0.004, math.Inf(1)}
	prev := &metrics.Float64Histogram{Counts: []uint64{5, 5, 0, 0}, Buckets: buckets}
	cur := &metrics.Float64Histogram{Counts: []uint64{5, 105, 0, 1}, Buckets: buckets}
	// 101 new observations: 100 in (1ms, 2ms], one unbounded. The 99th percentile falls
	// in the second bucket, reported at its upper bound.
	if v, ok := histP99(prev, cur); !ok || v != 0.002 {
		t.Fatalf("p99 = %v %v, want 0.002", v, ok)
	}
	cur2 := &metrics.Float64Histogram{Counts: []uint64{5, 5, 0, 50}, Buckets: buckets}
	if v, _ := histP99(prev, cur2); v != 0.004 {
		t.Fatalf("an unbounded bucket reports its lower bound: %v", v)
	}
	if _, ok := histP99(prev, prev); ok {
		t.Fatalf("no new observations, no quantile")
	}
}

func TestGeneratorSample(t *testing.T) {
	now := time.Unix(100, 0)
	g := NewGenerator(func() time.Time { return now }, func() map[string]RunnerHealth {
		return map[string]RunnerHealth{"http": {InFlight: 7, DispatchLagP99MS: 1.25, HasLag: true}, "db": {InFlight: 2}}
	})
	first, err := g.Sample(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if first["goroutines"].(int64) <= 0 || first["heap_bytes"].(int64) <= 0 {
		t.Fatalf("runtime values missing: %v", first)
	}
	inFlight := first["in_flight"].(map[string]any)
	lag := first["dispatch_lag_p99_ms"].(map[string]any)
	if inFlight["http"] != int64(7) || lag["http"] != 1.25 {
		t.Fatalf("runner health = %v / %v", inFlight, lag)
	}
	if _, has := lag["db"]; has {
		t.Fatalf("a runner with nothing dispatched reports no lag")
	}
	burn := 0
	for i := range 2_000_000 {
		burn += i % 7
	}
	_ = burn
	now = now.Add(time.Second)
	second, _ := g.Sample(context.Background(), time.Second)
	if _, ok := second["cpu_ratio"]; !ok && processCPUAvailable() {
		t.Fatalf("cpu_ratio missing on the second sample: %v", second)
	}
}

func processCPUAvailable() bool {
	_, ok := processCPU()
	return ok
}

func TestRedisSamplerAgainstMiniredis(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := New(Spec{Name: "redis", Redis: &config.Redis{Addr: mr.Addr()}, RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.(Opener).Open(ctx); err != nil {
		t.Skipf("miniredis does not serve INFO the way this sampler needs: %v", err)
	}
	defer func() { _ = s.(Closer).Close() }()
	if _, err := s.Sample(ctx, 0); err != nil {
		t.Fatalf("Sample: %v", err)
	}
}

func TestRedisFromDSN(t *testing.T) {
	r, err := RedisFromDSN("redis://user:secret@cache:6380/2")
	if err != nil || r.Addr != "cache:6380" || r.Username != "user" || r.Password != "secret" || r.DB != 2 {
		t.Fatalf("got %+v, %v", r, err)
	}
	if r, _ := RedisFromDSN("cache:6379"); r.Addr != "cache:6379" {
		t.Fatalf("bare address: %+v", r)
	}
	if _, err := RedisFromDSN("redis://%zz"); err == nil {
		t.Fatalf("a malformed URL must be refused")
	}
}

func TestRegistry(t *testing.T) {
	got := strings.Join(Registered(), ",")
	if got != "mysql,postgres,redis" {
		t.Fatalf("registered = %s", got)
	}
	if _, err := New(Spec{Name: "mongo"}); err == nil {
		t.Fatalf("an unknown sampler must be refused")
	}
	for _, name := range []string{"postgres", "mysql", "redis"} {
		if _, err := New(Spec{Name: name}); err == nil {
			t.Errorf("%s with nothing to connect to must be refused", name)
		}
	}
}
