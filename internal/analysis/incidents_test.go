package analysis

import (
	"reflect"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

func TestEpisodes(t *testing.T) {
	hot := func(idx ...int) []result.HotBucket {
		out := make([]result.HotBucket, len(idx))
		for i, v := range idx {
			out[i] = result.HotBucket{Index: v}
		}
		return out
	}
	cases := []struct {
		name string
		in   []int
		want []span
	}{
		{"none", nil, nil},
		{"single", []int{4}, []span{{4, 4}}},
		{"contiguous", []int{4, 5, 6}, []span{{4, 6}}},
		// A gap of one bucket is bridged; a gap of two is not.
		{"gap of one", []int{4, 6}, []span{{4, 6}}},
		{"gap of two", []int{4, 7}, []span{{4, 4}, {7, 7}}},
		{"mixed", []int{1, 2, 4, 9, 10, 20}, []span{{1, 4}, {9, 10}, {20, 20}}},
	}
	for _, c := range cases {
		got := episodes(hot(c.in...), 1)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: episodes = %v, want %v", c.name, got, c.want)
		}
	}
}

// Merging must be idempotent: merging already-merged episodes changes nothing, and
// the result covers exactly the input and never overlaps itself (§10).
func TestEpisodesProperties(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		set := rapid.SliceOfNDistinct(rapid.IntRange(0, 200), 0, 60, rapid.ID[int]).Draw(rt, "hot")
		sort.Ints(set)
		hot := make([]result.HotBucket, len(set))
		for i, v := range set {
			hot[i] = result.HotBucket{Index: v}
		}
		first := episodes(hot, 1)

		// Re-merge the spans' endpoints as if they were hot buckets.
		var again []result.HotBucket
		for _, s := range first {
			again = append(again, result.HotBucket{Index: s.start})
			if s.end != s.start {
				again = append(again, result.HotBucket{Index: s.end})
			}
		}
		// Endpoints alone can leave internal gaps wider than one, so re-merge the
		// full coverage instead.
		var cover []result.HotBucket
		for _, s := range first {
			for i := s.start; i <= s.end; i++ {
				cover = append(cover, result.HotBucket{Index: i})
			}
		}
		if second := episodes(cover, 1); !reflect.DeepEqual(first, second) {
			rt.Fatalf("not idempotent: %v then %v", first, second)
		}
		_ = again

		for i := 1; i < len(first); i++ {
			if first[i].start-first[i-1].end <= 2 {
				rt.Fatalf("episodes %v and %v should have merged", first[i-1], first[i])
			}
		}
		for _, v := range set {
			in := false
			for _, s := range first {
				if v >= s.start && v <= s.end {
					in = true
				}
			}
			if !in {
				rt.Fatalf("hot bucket %d is not covered by %v", v, first)
			}
		}
	})
}

// scenario is a three-tier run whose steady state is http 20ms, db 2ms, redis 0.5ms.
type scenario struct {
	n       int
	runners map[string][]float64 // service p99 per bucket
	wait    map[string][]float64 // client-wait p99 per bucket, default ~0
	insuff  map[string]map[int]bool
}

func newScenario(n int) *scenario {
	return &scenario{
		n: n,
		runners: map[string][]float64{
			"http": steady(n, 20), "db": steady(n, 2), "redis": steady(n, 0.5),
		},
		wait:   map[string][]float64{},
		insuff: map[string]map[int]bool{},
	}
}

func (s *scenario) set(runner string, from, to int, p99 float64) *scenario {
	for i := from; i <= to; i++ {
		s.runners[runner][i] = p99
	}
	return s
}

func (s *scenario) waitAt(runner string, from, to int, ms float64) *scenario {
	if s.wait[runner] == nil {
		s.wait[runner] = make([]float64, s.n)
	}
	for i := from; i <= to; i++ {
		s.wait[runner][i] = ms
	}
	return s
}

func (s *scenario) starve(runner string, from, to int) *scenario {
	if s.insuff[runner] == nil {
		s.insuff[runner] = map[int]bool{}
	}
	for i := from; i <= to; i++ {
		s.insuff[runner][i] = true
	}
	return s
}

func (s *scenario) drop(runner string) *scenario {
	delete(s.runners, runner)
	return s
}

func (s *scenario) result() *result.Result {
	r := &result.Result{
		SchemaVersion: result.SchemaVersion,
		Run:           result.Run{ID: "test", Status: result.StatusCompleted, BucketMS: 1000, DurationMS: float64(s.n) * 1000, MinSamples: 20},
	}
	for _, name := range []string{"http", "db", "redis"} {
		vals, ok := s.runners[name]
		if !ok {
			continue
		}
		kind := "storage"
		if name == "http" {
			kind = "app"
		}
		b := series(vals...)
		for i := range b {
			w := 0.1
			if s.wait[name] != nil {
				w = s.wait[name][i]
			}
			b[i].ClientWait = result.Quantiles{P99: w}
			b[i].Response = result.Quantiles{P50: vals[i] / 2, P99: vals[i] + w}
			if s.insuff[name][i] {
				b[i].Insufficient = true
				b[i].N = 2
			}
		}
		var sum result.OpSummary
		sum.N = int64(100 * s.n)
		r.Runners = append(r.Runners, result.Runner{Name: name, Kind: kind, Buckets: b, Summary: sum})
	}
	return r
}

func analyse(t *testing.T, r *result.Result) result.Analysis {
	t.Helper()
	return Analyse(r, Inputs{})
}

func only(t *testing.T, a result.Analysis) result.Incident {
	t.Helper()
	if len(a.Incidents) != 1 {
		t.Fatalf("incidents = %+v, want exactly one", a.Incidents)
	}
	return a.Incidents[0]
}

func TestIncidentCorrelated(t *testing.T) {
	// The database stalls at 30; the application follows a bucket later.
	r := newScenario(60).set("db", 30, 34, 900).set("http", 31, 35, 950).result()
	inc := only(t, analyse(t, r))
	if inc.Class != result.IncidentCorrelated {
		t.Fatalf("class = %s, want correlated", inc.Class)
	}
	if inc.StartIndex != 30 || inc.EndIndex != 35 {
		t.Fatalf("window = %d..%d, want 30..35", inc.StartIndex, inc.EndIndex)
	}
	if inc.Culprit == nil || inc.Culprit.Runner != "db" || len(inc.Culprit.TiedWith) != 0 {
		t.Fatalf("culprit = %+v, want db alone", inc.Culprit)
	}
	if inc.Culprit.LeadBuckets != 1 {
		t.Fatalf("lead = %d, want 1", inc.Culprit.LeadBuckets)
	}
	if inc.AppImpact == nil || inc.AppImpact.PeakP99MS < 950 {
		t.Fatalf("app impact = %+v", inc.AppImpact)
	}
	if inc.StartS != 30 || inc.EndS != 36 || inc.DurationS != 6 {
		t.Fatalf("seconds = %v..%v (%v)", inc.StartS, inc.EndS, inc.DurationS)
	}
}

// Two storage tiers hot at once, equally early and equally severe, is a tie - and a
// tie is reported as one, never broken by accident of ordering.
func TestIncidentCulpritTie(t *testing.T) {
	r := newScenario(60).
		set("db", 30, 33, 400).set("redis", 30, 33, 400).set("http", 30, 33, 900).result()
	inc := only(t, analyse(t, r))
	if inc.Culprit == nil || len(inc.Culprit.TiedWith) != 1 {
		t.Fatalf("culprit = %+v, want a two-way tie", inc.Culprit)
	}
	names := []string{inc.Culprit.Runner, inc.Culprit.TiedWith[0]}
	sort.Strings(names)
	if names[0] != "db" || names[1] != "redis" {
		t.Fatalf("tie between %v, want db and redis", names)
	}
}

// Lead time outweighs severity: the tier that moved first is the better candidate.
func TestIncidentCulpritLeadWins(t *testing.T) {
	r := newScenario(60).
		set("redis", 30, 34, 150).set("db", 31, 34, 400).set("http", 31, 34, 900).result()
	inc := only(t, analyse(t, r))
	if inc.Culprit == nil || inc.Culprit.Runner != "redis" {
		t.Fatalf("culprit = %+v, want redis, which led", inc.Culprit)
	}
}

func TestIncidentStorageOnly(t *testing.T) {
	r := newScenario(60).set("db", 30, 34, 900).result()
	inc := only(t, analyse(t, r))
	if inc.Class != result.IncidentStorageOnly {
		t.Fatalf("class = %s, want storage_only", inc.Class)
	}
	if inc.Culprit != nil {
		t.Fatalf("storage_only names no culprit among app causes: %+v", inc.Culprit)
	}
}

func TestIncidentAppOnly(t *testing.T) {
	r := newScenario(60).set("http", 30, 34, 900).result()
	inc := only(t, analyse(t, r))
	if inc.Class != result.IncidentAppOnly {
		t.Fatalf("class = %s, want app_only", inc.Class)
	}
	for _, rn := range inc.Runners {
		if rn.Name != "http" && (rn.Hot || rn.Insufficient) {
			t.Fatalf("storage runner %+v should be healthy on sufficient data", rn)
		}
	}
}

// app_only is an accusation, so it demands that the probes were actually watching. A
// quiet probe produces unobserved - the tool declining to answer - instead.
func TestIncidentUnobserved(t *testing.T) {
	r := newScenario(60).set("http", 30, 34, 900).starve("db", 28, 36).result()
	inc := only(t, analyse(t, r))
	if inc.Class != result.IncidentUnobserved {
		t.Fatalf("class = %s, want unobserved", inc.Class)
	}

	// With no probe at all there is nothing to exonerate the storage tiers with.
	r = newScenario(60).drop("db").drop("redis").set("http", 30, 34, 900).result()
	if inc := only(t, analyse(t, r)); inc.Class != result.IncidentUnobserved {
		t.Fatalf("http alone: class = %s, want unobserved", inc.Class)
	}
}

// When the generator's own waiting dominates, the target is not blamed.
func TestIncidentClientLimited(t *testing.T) {
	r := newScenario(60).set("http", 30, 34, 300).waitAt("http", 30, 34, 2000).result()
	inc := only(t, analyse(t, r))
	if inc.Class != result.IncidentClientLimited {
		t.Fatalf("class = %s, want client_limited", inc.Class)
	}

	// A storage stall that also backs up our queue is still the storage tier's fault:
	// only a hot tier that is not dominated by our waiting can carry the blame.
	r = newScenario(60).set("db", 30, 34, 900).
		set("http", 30, 34, 950).waitAt("http", 30, 34, 3000).result()
	if inc := only(t, analyse(t, r)); inc.Class != result.IncidentCorrelated {
		t.Fatalf("class = %s, want correlated", inc.Class)
	}
}

// Episodes of different runners overlap with a tolerance of one bucket: shifting one
// by a bucket either way must make them overlap. Adjacent episodes are therefore one
// incident, and episodes with a bucket between them are two.
func TestIncidentOverlapTolerance(t *testing.T) {
	r := newScenario(60).set("db", 30, 31, 900).set("http", 32, 33, 900).result()
	if a := analyse(t, r); len(a.Incidents) != 1 {
		t.Fatalf("adjacent: %d incidents, want 1", len(a.Incidents))
	}
	r = newScenario(60).set("db", 30, 31, 900).set("http", 33, 34, 900).result()
	a := analyse(t, r)
	if len(a.Incidents) != 2 {
		t.Fatalf("one bucket apart: %d incidents, want 2", len(a.Incidents))
	}
	if a.Incidents[0].ID != "inc-1" || a.Incidents[1].ID != "inc-2" ||
		a.Incidents[0].Class != result.IncidentStorageOnly || a.Incidents[1].Class != result.IncidentAppOnly {
		t.Fatalf("incidents = %+v", a.Incidents)
	}
}

func TestCleanRunHasNoIncidents(t *testing.T) {
	a := analyse(t, newScenario(120).result())
	if len(a.Incidents) != 0 || len(a.HotBuckets) != 0 {
		t.Fatalf("clean run: %d incidents, hot %v", len(a.Incidents), a.HotBuckets)
	}
	if a.Verdict.Bottleneck != result.BottleneckNone {
		t.Fatalf("verdict = %s, want none", a.Verdict.Bottleneck)
	}
}

// Telemetry that moves inside the window corroborates the culprit and raises it.
func TestIncidentTelemetryCorroboration(t *testing.T) {
	r := newScenario(60).set("db", 30, 34, 900).set("http", 30, 34, 950).result()
	var samples []map[string]any
	for i := range 60 {
		locks := 0.0
		if i >= 30 && i <= 34 {
			locks = 12
		}
		samples = append(samples, map[string]any{"t_ms": float64(i * 1000), "locks_waiting": locks, "sample_ms": 1.0})
	}
	r.Telemetry = &result.Telemetry{Postgres: &result.SamplerSeries{Available: true, Samples: samples}}
	inc := only(t, analyse(t, r))
	if len(inc.Telemetry) == 0 {
		t.Fatalf("no telemetry signal found")
	}
	sig := inc.Telemetry[0]
	if sig.Source != "postgres" || sig.Signal != "locks_waiting" || sig.Value != 12 || sig.Baseline != 0 {
		t.Fatalf("signal = %+v", sig)
	}
	if inc.Culprit == nil || inc.Culprit.Runner != "db" {
		t.Fatalf("culprit = %+v", inc.Culprit)
	}
	found := false
	for _, reason := range inc.Culprit.Reasons {
		if reason == "corroborated by postgres locks_waiting" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want the corroboration named", inc.Culprit.Reasons)
	}
	if inc.Confidence != result.ConfidenceHigh {
		t.Fatalf("confidence = %s, want high", inc.Confidence)
	}
}

// Redis DEBUG SLEEP stalls the server, so the INFO call itself takes as long as the
// sleep and throughput collapses: both show up as signals.
func TestIncidentRedisStallSignals(t *testing.T) {
	r := newScenario(60).set("redis", 30, 32, 2500).set("http", 30, 32, 2600).result()
	var samples []map[string]any
	for i := range 60 {
		ops, took := 500.0, 0.4
		if i >= 30 && i <= 32 {
			ops, took = 3, 2400
		}
		samples = append(samples, map[string]any{"t_ms": float64(i * 1000), "ops_per_sec": ops, "sample_ms": took})
	}
	r.Telemetry = &result.Telemetry{Redis: &result.SamplerSeries{Available: true, Samples: samples}}
	inc := only(t, analyse(t, r))
	got := map[string]bool{}
	for _, s := range inc.Telemetry {
		got[s.Source+"/"+s.Signal] = true
	}
	if !got["redis/ops_per_sec"] || !got["redis/sample_ms"] {
		t.Fatalf("signals = %+v", inc.Telemetry)
	}
	if inc.Culprit == nil || inc.Culprit.Runner != "redis" {
		t.Fatalf("culprit = %+v", inc.Culprit)
	}
}

// Analysis is a pure function: the same document always yields the same analysis.
func TestAnalyseDeterministic(t *testing.T) {
	build := func() *result.Result {
		return newScenario(90).set("db", 30, 34, 900).set("http", 31, 35, 950).set("redis", 60, 61, 90).result()
	}
	a, b := analyse(t, build()), analyse(t, build())
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("two analyses of the same data differ")
	}
}
