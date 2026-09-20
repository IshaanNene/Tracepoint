package metrics_test

import (
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"pgregory.net/rapid"

	"github.com/IshaanNene/Tracepoint/internal/metrics"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func testConfig(labels ...string) metrics.Config {
	if len(labels) == 0 {
		labels = []string{"probe"}
	}
	return metrics.Config{
		Runner:      "http",
		Kind:        metrics.KindApp,
		Labels:      labels,
		BucketWidth: time.Second,
		SealDelay:   10 * time.Second,
		MinSamples:  20,
	}
}

func newCollector(t *testing.T, cfg metrics.Config) *metrics.Collector {
	t.Helper()
	c, err := metrics.NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return c
}

// outcome builds a completed operation whose timings decompose cleanly:
// intended -> (clientWait) -> connection acquired -> (serviceTime) -> end.
func outcome(label metrics.LabelID, intended, clientWait, service time.Duration) *metrics.Outcome {
	conn := intended + clientWait
	return &metrics.Outcome{
		Label:        label,
		Class:        metrics.ClassOK,
		Status:       200,
		Intended:     intended,
		Dispatched:   intended + clientWait/3,
		WorkerStart:  intended + 2*clientWait/3,
		ConnAcquired: conn,
		FirstByte:    conn + service/2,
		End:          conn + service,
		BytesIn:      100,
	}
}

// The three latency views must decompose exactly, because the whole attribution
// story rests on separating what the target did from what we cost ourselves.
func TestLatencyDecomposition(t *testing.T) {
	t.Parallel()
	o := outcome(0, 5*time.Second, 30*time.Millisecond, 120*time.Millisecond)
	if got, want := o.ServiceTime(), 120*time.Millisecond; got != want {
		t.Errorf("ServiceTime() = %v, want %v", got, want)
	}
	if got, want := o.ClientWait(), 30*time.Millisecond; got != want {
		t.Errorf("ClientWait() = %v, want %v", got, want)
	}
	if got, want := o.ResponseTime(), 150*time.Millisecond; got != want {
		t.Errorf("ResponseTime() = %v, want %v", got, want)
	}
	if o.ResponseTime() != o.ServiceTime()+o.ClientWait() {
		t.Error("response time must equal service time plus client wait")
	}
}

// The abort guard counts connection errors, timeouts and 5xx, and must not count
// 4xx or 429 - those are the target working as designed (spec §8).
func TestAbortGuardClassification(t *testing.T) {
	t.Parallel()
	counts := map[metrics.Class]bool{
		metrics.ClassTimeout:       true,
		metrics.ClassConnection:    true,
		metrics.ClassHTTP5xx:       true,
		metrics.ClassDNS:           true,
		metrics.ClassTLS:           true,
		metrics.ClassHTTP4xx:       false,
		metrics.ClassOK:            false,
		metrics.ClassExpectFailed:  false,
		metrics.ClassCanceled:      false,
		metrics.ClassPoolTimeout:   false,
		metrics.ClassExtractFailed: false,
	}
	for class, want := range counts {
		if got := class.CountsTowardAbortGuard(); got != want {
			t.Errorf("%s.CountsTowardAbortGuard() = %v, want %v", class, got, want)
		}
	}
	if metrics.ClassOK.IsError() {
		t.Error("ClassOK must not be an error")
	}
	if !metrics.ClassTimeout.IsError() {
		t.Error("ClassTimeout must be an error")
	}
	// Every class must render as the snake_case name the result schema uses.
	if got := metrics.ClassHTTP5xx.String(); got != "http_5xx" {
		t.Errorf("ClassHTTP5xx.String() = %q, want %q", got, "http_5xx")
	}
}

func TestBucketsUseTheIntendedTimeNotTheEndTime(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	// Scheduled in bucket 2, but it took four seconds and finished in bucket 6. It
	// belongs to bucket 2: that is the second whose load it represents, and counting
	// it later would hide the stall from the second that caused it.
	c.Record(outcome(0, 2500*time.Millisecond, 0, 4*time.Second))
	c.Finish(60 * time.Second)

	snap := c.Snapshot()
	var found bool
	for _, b := range snap.Buckets {
		if b.N > 0 {
			if b.Index != 2 {
				t.Errorf("operation landed in bucket %d, want 2 (its intended second)", b.Index)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("the operation was not recorded in any bucket")
	}
}

func TestQuantilesAreAccurateWithinTheSketchGuarantee(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	rng := rand.New(rand.NewPCG(9, 9))

	exact := make([]float64, 0, 20_000)
	for i := range 20_000 {
		// A realistic right-skewed latency spread: mostly fast, with a real tail.
		ms := 5 + rng.Float64()*15
		if i%100 == 0 {
			ms = 200 + rng.Float64()*600
		}
		exact = append(exact, ms)
		d := time.Duration(ms * float64(time.Millisecond))
		c.Record(outcome(0, time.Duration(i)*time.Millisecond, 0, d))
	}
	c.Finish(10 * time.Minute)
	got := c.Snapshot().Summary.Service

	for _, tc := range []struct {
		name string
		q    float64
		got  float64
	}{
		{"p50", 0.50, got.P50},
		{"p90", 0.90, got.P90},
		{"p95", 0.95, got.P95},
		{"p99", 0.99, got.P99},
	} {
		want := exactQuantile(exact, tc.q)
		if relErr := math.Abs(tc.got-want) / want; relErr > 0.01 {
			t.Errorf("%s = %.3fms, exact %.3fms, relative error %.4f exceeds the 1%% guarantee",
				tc.name, tc.got, want, relErr)
		}
	}
	// Max and mean are tracked exactly rather than read from the sketch.
	if want := maxOf(exact); math.Abs(got.Max-want) > 1e-6 {
		t.Errorf("Max = %v, want the exact %v", got.Max, want)
	}
	if want := meanOf(exact); math.Abs(got.Mean-want) > 1e-6 {
		t.Errorf("Mean = %v, want the exact %v", got.Mean, want)
	}
}

func TestWarmupIsExcludedFromSummariesButKeptOnTheTimeline(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Warmup = 5 * time.Second
	c := newCollector(t, cfg)

	for i := range 100 { // inside warm-up, deliberately slow
		c.Record(outcome(0, time.Duration(i)*40*time.Millisecond, 0, 900*time.Millisecond))
	}
	for i := range 100 { // after warm-up, fast
		c.Record(outcome(0, 6*time.Second+time.Duration(i)*40*time.Millisecond, 0, 10*time.Millisecond))
	}
	c.Finish(time.Minute)
	snap := c.Snapshot()

	if snap.Summary.N != 100 {
		t.Errorf("summary counted %d operations, want only the 100 after warm-up", snap.Summary.N)
	}
	if snap.Summary.Service.P50 > 50 {
		t.Errorf("summary p50 = %vms; warm-up latency leaked into it", snap.Summary.Service.P50)
	}
	var warm, steady int
	for _, b := range snap.Buckets {
		if b.N == 0 {
			continue
		}
		if b.Warmup {
			warm++
		} else {
			steady++
		}
	}
	if warm == 0 {
		t.Error("warm-up buckets must still appear on the timeline, shaded rather than hidden")
	}
	if steady == 0 {
		t.Error("no steady buckets recorded")
	}
}

// A p99 from eleven samples is the maximum of eleven samples wearing a percentile's
// name. Such buckets are displayed but must be marked so analysis never uses them.
func TestBucketsBelowMinSamplesAreMarkedInsufficient(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MinSamples = 20
	c := newCollector(t, cfg)

	for i := range 5 { // bucket 0: too few
		c.Record(outcome(0, time.Duration(i)*time.Millisecond, 0, 10*time.Millisecond))
	}
	for i := range 40 { // bucket 1: enough
		c.Record(outcome(0, time.Second+time.Duration(i)*time.Millisecond, 0, 10*time.Millisecond))
	}
	c.Finish(time.Minute)

	byIndex := map[int]metrics.BucketSummary{}
	for _, b := range c.Snapshot().Buckets {
		byIndex[b.Index] = b
	}
	if !byIndex[0].Insufficient {
		t.Error("a bucket with 5 samples must be marked insufficient")
	}
	if byIndex[1].Insufficient {
		t.Error("a bucket with 40 samples must not be marked insufficient")
	}
}

func TestErrorClassesAreCountedSeparately(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	classes := []metrics.Class{
		metrics.ClassOK, metrics.ClassOK, metrics.ClassOK,
		metrics.ClassTimeout, metrics.ClassTimeout,
		metrics.ClassHTTP5xx,
		metrics.ClassConnection,
	}
	for i, class := range classes {
		o := outcome(0, time.Duration(i)*time.Millisecond, 0, 10*time.Millisecond)
		o.Class = class
		c.Record(o)
	}
	c.Finish(time.Minute)
	s := c.Snapshot().Summary

	if s.N != 7 || s.OK != 3 || s.ErrorsTotal != 4 {
		t.Errorf("n=%d ok=%d errors=%d, want 7/3/4", s.N, s.OK, s.ErrorsTotal)
	}
	if math.Abs(s.ErrorRatio-4.0/7.0) > 1e-9 {
		t.Errorf("error ratio = %v, want %v", s.ErrorRatio, 4.0/7.0)
	}
	for class, want := range map[string]int64{"timeout": 2, "http_5xx": 1, "connection": 1} {
		if got := s.Errors[class]; got != want {
			t.Errorf("errors[%s] = %d, want %d", class, got, want)
		}
	}
	if _, present := s.Errors["ok"]; present {
		t.Error("successful operations must not appear in the error counts")
	}
	if _, present := s.Errors["dns"]; present {
		t.Error("a class with no occurrences must be absent, not zero")
	}
}

func TestPerLabelStatistics(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig("list-items", "create-item"))
	list, create := c.LabelID("list-items"), c.LabelID("create-item")

	for i := range 60 {
		c.Record(outcome(list, time.Duration(i)*10*time.Millisecond, 0, 10*time.Millisecond))
	}
	for i := range 30 {
		c.Record(outcome(create, time.Duration(i)*10*time.Millisecond, 0, 200*time.Millisecond))
	}
	c.Finish(time.Minute)
	snap := c.Snapshot()

	if got := snap.LabelSummaries["list-items"].N; got != 60 {
		t.Errorf("list-items n = %d, want 60", got)
	}
	if got := snap.LabelSummaries["create-item"].N; got != 30 {
		t.Errorf("create-item n = %d, want 30", got)
	}
	if fast, slow := snap.LabelSummaries["list-items"].Service.P50, snap.LabelSummaries["create-item"].Service.P50; fast >= slow {
		t.Errorf("labels were not kept apart: list p50 %v, create p50 %v", fast, slow)
	}
	if snap.Summary.N != 90 {
		t.Errorf("runner total = %d, want 90", snap.Summary.N)
	}
}

// Labels are capped so a caller that accidentally puts an id in a name cannot make
// memory grow without bound; the overflow is collected rather than dropped.
func TestLabelOverflowCollectsIntoOther(t *testing.T) {
	t.Parallel()
	labels := make([]string, 60)
	for i := range labels {
		labels[i] = "label-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	c := newCollector(t, testConfig(labels...))

	for i, name := range labels {
		id := c.LabelID(name)
		c.Record(outcome(id, time.Duration(i)*time.Millisecond, 0, 10*time.Millisecond))
	}
	c.Finish(time.Minute)
	snap := c.Snapshot()

	// Fifty named labels plus the catch-all; the cap is on names, and "other" is an
	// extra bucket rather than one of the fifty.
	if len(snap.Labels) > metrics.MaxLabels+1 {
		t.Errorf("kept %d labels, want at most %d plus the overflow bucket", len(snap.Labels), metrics.MaxLabels)
	}
	if snap.Labels[len(snap.Labels)-1] != "other" {
		t.Errorf("last label = %q, want the overflow bucket %q", snap.Labels[len(snap.Labels)-1], "other")
	}
	if snap.LabelSummaries["other"].N == 0 {
		t.Error("labels beyond the cap must be collected under \"other\", not dropped")
	}
	if snap.Summary.N != int64(len(labels)) {
		t.Errorf("runner total = %d, want %d; overflow must still count", snap.Summary.N, len(labels))
	}
	if !snap.LabelOverflow {
		t.Error("the snapshot must report that the label cap was reached")
	}
}

// Sealing is what keeps memory flat. A bucket may only seal once nothing in flight
// can still land in it, which is its end plus the longest operation timeout.
func TestSealingWaitsForTheTimeoutThenFreesMemory(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.BucketWidth = time.Second
	cfg.SealDelay = 10 * time.Second
	c := newCollector(t, cfg)

	for i := range 30 {
		c.Record(outcome(0, time.Duration(i)*30*time.Millisecond, 0, 5*time.Millisecond))
	}
	// Bucket 0 ends at 1s; it may not seal before 11s.
	c.SealThrough(10 * time.Second)
	if got := c.OpenBuckets(); got == 0 {
		t.Error("bucket 0 sealed before its end plus the operation timeout")
	}
	c.SealThrough(11500 * time.Millisecond)
	if got := c.OpenBuckets(); got != 0 {
		t.Errorf("%d buckets still open after the seal deadline passed", got)
	}
	c.Finish(time.Minute)
	if n := len(c.Snapshot().Buckets); n == 0 {
		t.Error("sealed buckets must survive as summaries")
	}
}

// An operation that lands after its bucket has already sealed must still count
// towards the run totals rather than vanishing.
func TestLateOutcomeIsCountedNotLost(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	c.Record(outcome(0, 0, 0, 5*time.Millisecond))
	c.SealThrough(time.Minute) // bucket 0 is now closed
	c.Record(outcome(0, 100*time.Millisecond, 0, 5*time.Millisecond))
	c.Finish(2 * time.Minute)

	snap := c.Snapshot()
	if snap.Summary.N != 2 {
		t.Errorf("summary n = %d, want 2; a late outcome must still be counted", snap.Summary.N)
	}
	if snap.Late != 1 {
		t.Errorf("late = %d, want 1", snap.Late)
	}
}

func TestExecutorCountersRideTheSameTimeline(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	for i := range 100 {
		c.RecordOffered(0)
		if i%10 == 0 {
			c.RecordDropped(0)
		}
	}
	c.ObserveInFlight(0, 37)
	c.ObserveInFlight(0, 12)
	c.Record(outcome(0, 0, 0, 5*time.Millisecond))
	c.Finish(time.Minute)

	b := c.Snapshot().Buckets[0]
	if b.Offered != 100 || b.Dropped != 10 {
		t.Errorf("offered=%d dropped=%d, want 100/10", b.Offered, b.Dropped)
	}
	if b.InFlightMax != 37 {
		t.Errorf("in-flight max = %d, want the peak 37", b.InFlightMax)
	}
}

func TestSerialisedSketchesRoundTrip(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig("a"))
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range 5_000 {
		ms := 1 + rng.Float64()*500
		c.Record(outcome(c.LabelID("a"), time.Duration(i)*time.Millisecond, 0, time.Duration(ms*float64(time.Millisecond))))
	}
	c.Finish(time.Hour)
	snap := c.Snapshot()

	enc, ok := snap.Sketches[metrics.RunnerSketchKey]
	if !ok {
		t.Fatalf("no runner-wide sketch; got keys %v", keysOf(snap.Sketches))
	}
	if enc.Count != 5_000 {
		t.Errorf("encoded count = %v, want 5000", enc.Count)
	}
	if enc.RelativeAccuracy != 0.01 {
		t.Errorf("relative accuracy = %v, want 0.01", enc.RelativeAccuracy)
	}
	back, err := metrics.DecodeSketch(enc)
	if err != nil {
		t.Fatalf("DecodeSketch: %v", err)
	}
	// compare depends on reading a quantile back out of this, so it must survive.
	got, err := back.GetValueAtQuantile(0.99)
	if err != nil {
		t.Fatalf("GetValueAtQuantile: %v", err)
	}
	if math.Abs(got-snap.Summary.Response.P99) > 1e-6 {
		t.Errorf("round-tripped p99 = %v, want %v", got, snap.Summary.Response.P99)
	}
}

// Property: merging sketches is associative and commutative, which is what makes
// per-shard and per-bucket merging sound regardless of the order they are read in.
func TestPropertySketchMergeIsOrderIndependent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		groups := rapid.SliceOfN(rapid.SliceOfN(rapid.Float64Range(0.1, 5000), 1, 40), 2, 5).Draw(rt, "groups")

		collect := func(order []int) metrics.Quantiles {
			c, err := metrics.NewCollector(testConfig())
			if err != nil {
				rt.Fatalf("NewCollector: %v", err)
			}
			at := time.Duration(0)
			for _, g := range order {
				for _, ms := range groups[g] {
					c.Record(outcome(0, at, 0, time.Duration(ms*float64(time.Millisecond))))
					at += time.Millisecond
				}
			}
			c.Finish(time.Hour)
			return c.Snapshot().Summary.Service
		}

		forward := make([]int, len(groups))
		for i := range forward {
			forward[i] = i
		}
		backward := make([]int, len(groups))
		for i := range backward {
			backward[i] = len(groups) - 1 - i
		}

		a, b := collect(forward), collect(backward)
		if a.P50 != b.P50 || a.P99 != b.P99 || a.Max != b.Max {
			rt.Fatalf("merge order changed the result: %+v vs %+v", a, b)
		}
	})
}

// Property: quantiles read from the sketch stay inside the configured relative
// accuracy of the exact answer, for any sample set.
func TestPropertyQuantileErrorWithinGuarantee(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		vals := rapid.SliceOfN(rapid.Float64Range(0.5, 10_000), 200, 2_000).Draw(rt, "latencies")
		c, err := metrics.NewCollector(testConfig())
		if err != nil {
			rt.Fatalf("NewCollector: %v", err)
		}
		for i, ms := range vals {
			c.Record(outcome(0, time.Duration(i)*time.Millisecond, 0, time.Duration(ms*float64(time.Millisecond))))
		}
		c.Finish(time.Hour)
		got := c.Snapshot().Summary.Service

		for _, tc := range []struct {
			q   float64
			got float64
		}{{0.5, got.P50}, {0.9, got.P90}, {0.99, got.P99}} {
			if !withinAccuracyOfNeighbouringRank(vals, tc.q, tc.got, 0.01) {
				rt.Fatalf("q%.2f = %v is not within 1%% of any order statistic adjacent to rank %v (n=%d)",
					tc.q, tc.got, tc.q*float64(len(vals)), len(vals))
			}
		}
	})
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*metrics.Config)
	}{
		{"no bucket width", func(c *metrics.Config) { c.BucketWidth = 0 }},
		{"negative bucket width", func(c *metrics.Config) { c.BucketWidth = -time.Second }},
		{"negative warmup", func(c *metrics.Config) { c.Warmup = -time.Second }},
		{"negative seal delay", func(c *metrics.Config) { c.SealDelay = -time.Second }},
		{"no runner name", func(c *metrics.Config) { c.Runner = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			tc.mutate(&cfg)
			if _, err := metrics.NewCollector(cfg); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// The record path must not allocate: a load generator that allocates per operation
// spends its tail latency in the garbage collector and measures itself.
func BenchmarkRecord(b *testing.B) {
	c, err := metrics.NewCollector(testConfig())
	if err != nil {
		b.Fatalf("NewCollector: %v", err)
	}
	outcomes := make([]metrics.Outcome, 1024)
	for i := range outcomes {
		outcomes[i] = *outcome(0, time.Duration(i)*time.Microsecond, time.Millisecond, time.Duration(i%400)*time.Millisecond)
	}
	// Warm the sketch stores so the benchmark measures steady state, which is what
	// the "amortised" in "amortised 0 allocs/op" refers to.
	for i := range outcomes {
		c.Record(&outcomes[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		c.Record(&outcomes[i&1023])
		i++
	}
}

func BenchmarkRecordParallel(b *testing.B) {
	c, err := metrics.NewCollector(testConfig("a", "b", "c", "d"))
	if err != nil {
		b.Fatalf("NewCollector: %v", err)
	}
	base := *outcome(0, 0, time.Millisecond, 20*time.Millisecond)
	for i := range 4096 {
		o := base
		o.Label = metrics.LabelID(i % 4)
		c.Record(&o)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var counter int64
	var mu sync.Mutex
	b.RunParallel(func(pb *testing.PB) {
		mu.Lock()
		counter++
		seed := counter
		mu.Unlock()
		o := base
		i := seed
		for pb.Next() {
			o.Label = metrics.LabelID(i % 4)
			o.Intended = time.Duration(i) * time.Microsecond
			c.Record(&o)
			i++
		}
	})
}

// withinAccuracyOfNeighbouringRank is the honest statement of what a rank-approximating
// sketch guarantees: the value it returns is within its relative accuracy of some order
// statistic whose rank is within one of the one requested.
//
// The looseness is not slack, it is a real difference in convention. DDSketch resolves
// a quantile at rank q*(n-1); the nearest-rank definition uses ceil(q*n). Those differ
// by at most one rank, which is invisible on continuous data and enormous on a step -
// 199 samples at 1ms followed by 3 at 10s put a four-order-of-magnitude cliff between
// two adjacent ranks. Pinning the test to one convention would fail the sketch for
// being correct under the other.
func withinAccuracyOfNeighbouringRank(vals []float64, q, got, accuracy float64) bool {
	s := append([]float64(nil), vals...)
	sortFloats(s)
	centre := int(math.Ceil(q*float64(len(s)))) - 1
	for _, idx := range []int{centre - 1, centre, centre + 1} {
		if idx < 0 || idx >= len(s) {
			continue
		}
		if want := s[idx]; want > 0 && math.Abs(got-want)/want <= accuracy {
			return true
		}
	}
	return false
}

func exactQuantile(vals []float64, q float64) float64 {
	s := append([]float64(nil), vals...)
	sortFloats(s)
	idx := int(math.Ceil(q*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func sortFloats(s []float64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func meanOf(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestClassesAndDispatchLag(t *testing.T) {
	t.Parallel()
	classes := metrics.Classes()
	if len(classes) < 13 {
		t.Errorf("Classes() returned %d classes, want the full taxonomy", len(classes))
	}
	seen := map[string]bool{}
	for _, c := range classes {
		name := c.String()
		if name == "" {
			t.Errorf("class %d has no name", c)
		}
		if seen[name] {
			t.Errorf("duplicate class name %q", name)
		}
		seen[name] = true
	}
	// Out-of-range values must degrade to "other" rather than panicking: the class is
	// written into result.json and a panic in a renderer would lose the whole run.
	if got := metrics.Class(200).String(); got != "other" {
		t.Errorf("Class(200).String() = %q, want %q", got, "other")
	}
	if !metrics.Class(200).IsError() {
		t.Error("an unknown class must count as an error, not a success")
	}
	if metrics.Class(200).CountsTowardAbortGuard() {
		t.Error("an unknown class must not trip the abort guard")
	}

	o := outcome(0, time.Second, 40*time.Millisecond, 10*time.Millisecond)
	o.Dispatched = time.Second + 12*time.Millisecond
	if got, want := o.DispatchLag(), 12*time.Millisecond; got != want {
		t.Errorf("DispatchLag() = %v, want %v", got, want)
	}
}

func TestStatusHistogram(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.TrackStatuses = true
	c := newCollector(t, cfg)

	for i, status := range []int32{200, 200, 200, 429, 429, 503, 0, 9000} {
		o := outcome(0, time.Duration(i)*time.Millisecond, 0, 10*time.Millisecond)
		o.Status = status
		c.Record(o)
	}
	c.Finish(time.Minute)
	h := c.Snapshot().StatusHistogram

	for code, want := range map[string]int64{"200": 3, "429": 2, "503": 1} {
		if h[code] != want {
			t.Errorf("status %s = %d, want %d", code, h[code], want)
		}
	}
	if _, present := h["0"]; present {
		t.Error("a zero status means a non-HTTP operation and must not appear")
	}
	if h["other"] != 1 {
		t.Errorf("out-of-range status: other = %d, want 1", h["other"])
	}
}

func TestStatusHistogramAbsentWhenNotTracked(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig()) // TrackStatuses defaults to false
	o := outcome(0, 0, 0, time.Millisecond)
	o.Status = 200
	c.Record(o)
	c.Finish(time.Minute)
	if h := c.Snapshot().StatusHistogram; h != nil {
		t.Errorf("status histogram = %v, want nil for a non-HTTP runner", h)
	}
}

func TestOrdinaryRunDoesNotReportAnOverflowLabel(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig("a", "b"))
	c.Record(outcome(c.LabelID("a"), 0, 0, time.Millisecond))
	c.Finish(time.Minute)
	snap := c.Snapshot()

	for _, l := range snap.Labels {
		if l == "other" {
			t.Error("a run with no overflow must not report an \"other\" label")
		}
	}
	if snap.LabelOverflow {
		t.Error("LabelOverflow set on a run that did not overflow")
	}
}

func TestUnknownLabelFallsIntoOverflow(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig("known"))
	if got, want := c.LabelID("never-configured"), c.LabelID("other"); got != want {
		t.Errorf("unknown label resolved to %d, want the overflow id %d", got, want)
	}
	// An out-of-range id must be rewritten rather than indexing out of bounds.
	o := outcome(metrics.LabelID(9999), 0, 0, time.Millisecond)
	c.Record(o)
	c.Finish(time.Minute)
	if c.Snapshot().Summary.N != 1 {
		t.Error("an outcome with an out-of-range label was dropped")
	}
}

func TestPerLabelTimelineBudget(t *testing.T) {
	t.Parallel()
	cfg := testConfig("a", "b")
	cfg.LabelTimelineBudget = 3 // tiny, so the budget is reached immediately
	c := newCollector(t, cfg)

	for i := range 40 {
		c.Record(outcome(c.LabelID("a"), time.Duration(i)*time.Second, 0, time.Millisecond))
		c.Record(outcome(c.LabelID("b"), time.Duration(i)*time.Second, 0, time.Millisecond))
	}
	c.Finish(time.Hour)
	snap := c.Snapshot()

	if !snap.LabelTimelineDropped {
		t.Error("the per-label timeline budget was exceeded but not reported")
	}
	if len(snap.LabelBuckets) != 0 {
		t.Errorf("per-label timelines retained (%d labels) after the budget was exceeded", len(snap.LabelBuckets))
	}
	// Dropping a timeline must not cost a single observation: whole-run figures and
	// the per-runner timeline are what every analysis actually reads.
	if snap.Summary.N != 80 {
		t.Errorf("summary n = %d, want 80; dropping timelines must not lose data", snap.Summary.N)
	}
	if snap.LabelSummaries["a"].N != 40 || snap.LabelSummaries["b"].N != 40 {
		t.Error("whole-run per-label summaries must survive the timeline budget")
	}
	if len(snap.Buckets) == 0 {
		t.Error("the per-runner timeline must survive the per-label budget")
	}
}

func TestPerLabelTimelineKeptWhenWithinBudget(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig("a", "b"))
	for i := range 5 {
		c.Record(outcome(c.LabelID("a"), time.Duration(i)*time.Second, 0, time.Millisecond))
		c.Record(outcome(c.LabelID("b"), time.Duration(i)*time.Second, 0, 2*time.Millisecond))
	}
	c.Finish(time.Hour)
	snap := c.Snapshot()

	if snap.LabelTimelineDropped {
		t.Fatal("timelines dropped although the budget was ample")
	}
	if got := len(snap.LabelBuckets["a"]); got != 5 {
		t.Errorf("label a has %d bucket summaries, want 5", got)
	}
	// They must come back in time order, since a chart and a rolling median both
	// assume it.
	prev := -1
	for _, b := range snap.LabelBuckets["a"] {
		if b.Index <= prev {
			t.Fatalf("per-label timeline is out of order at index %d", b.Index)
		}
		prev = b.Index
	}
}

// Gaps in the timeline are filled rather than omitted: the analysis reads "the
// previous 30 buckets", and a hole would silently shift that window.
func TestTimelineIsContiguous(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	c.Record(outcome(0, 0, 0, time.Millisecond))
	c.Record(outcome(0, 5*time.Second, 0, time.Millisecond))
	c.Finish(time.Minute)

	buckets := c.Snapshot().Buckets
	if len(buckets) != 6 {
		t.Fatalf("got %d buckets, want 6 contiguous from 0 to 5", len(buckets))
	}
	for i, b := range buckets {
		if b.Index != i {
			t.Errorf("bucket %d has index %d; the timeline must be contiguous", i, b.Index)
		}
		if b.Errors == nil {
			t.Errorf("bucket %d has a nil error map; renderers should not have to nil-check", i)
		}
	}
	if buckets[3].N != 0 || !buckets[3].Insufficient {
		t.Error("a filled gap must report zero samples and be marked insufficient")
	}
}

func TestNegativeTimingsAreClampedNotPropagated(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	// A malformed outcome - end before the connection was acquired - must not poison
	// the sketch or produce a negative percentile in a report.
	o := outcome(0, time.Second, 10*time.Millisecond, 0)
	o.End = o.ConnAcquired - 5*time.Millisecond
	c.Record(o)
	c.Finish(time.Minute)

	q := c.Snapshot().Summary.Service
	if q.P50 < 0 || q.P99 < 0 || q.Max < 0 {
		t.Errorf("negative timings reached the summary: %+v", q)
	}
}

func TestDecodeSketchRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := metrics.DecodeSketch(metrics.EncodedSketch{Encoding: "protobuf-v9", Data: "AA=="}); err == nil {
		t.Error("an unknown encoding must be rejected")
	}
	if _, err := metrics.DecodeSketch(metrics.EncodedSketch{Encoding: metrics.SketchEncoding, Data: "not base64!!"}); err == nil {
		t.Error("malformed base64 must be rejected")
	}
	if _, err := metrics.DecodeSketch(metrics.EncodedSketch{Encoding: metrics.SketchEncoding, Data: "3q2+7w=="}); err == nil {
		t.Error("a corrupt payload must be rejected")
	}
}

func TestEmptyCollectorSnapshotIsUsable(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	c.Finish(time.Minute)
	snap := c.Snapshot()

	if snap.Summary.N != 0 || snap.Summary.ErrorRatio != 0 {
		t.Errorf("empty summary = %+v, want zeroes", snap.Summary)
	}
	if len(snap.Buckets) != 0 {
		t.Errorf("empty run produced %d buckets", len(snap.Buckets))
	}
	if snap.Summary.AchievedRPS != 0 {
		t.Errorf("empty run reported %v rps", snap.Summary.AchievedRPS)
	}
	// Even with nothing recorded the runner sketch must exist, so compare has
	// something well-formed to read.
	if _, ok := snap.Sketches[metrics.RunnerSketchKey]; !ok {
		t.Error("the runner sketch is missing from an empty snapshot")
	}
}

func TestAchievedRPSUsesTheMeasuredWindow(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Warmup = 2 * time.Second
	c := newCollector(t, cfg)

	// 2s of warm-up, then 4 seconds at 10/s.
	for i := range 20 {
		c.Record(outcome(0, time.Duration(i)*100*time.Millisecond, 0, time.Millisecond))
	}
	for s := 2; s < 6; s++ {
		for i := range 10 {
			c.Record(outcome(0, time.Duration(s)*time.Second+time.Duration(i)*99*time.Millisecond, 0, time.Millisecond))
		}
	}
	c.Finish(time.Minute)

	snap := c.Snapshot()
	if snap.Summary.N != 40 {
		t.Fatalf("n = %d, want the 40 post-warm-up operations", snap.Summary.N)
	}
	if got := snap.Summary.AchievedRPS; got < 9.5 || got > 10.5 {
		t.Errorf("achieved rps = %v, want about 10 over the four measured seconds", got)
	}
}

func TestNegativeOffsetLandsInTheFirstBucket(t *testing.T) {
	t.Parallel()
	c := newCollector(t, testConfig())
	if got := c.BucketIndexOf(-time.Second); got != 0 {
		t.Errorf("BucketIndexOf(-1s) = %d, want 0", got)
	}
	if got := c.BucketIndexOf(2500 * time.Millisecond); got != 2 {
		t.Errorf("BucketIndexOf(2.5s) = %d, want 2", got)
	}
}
