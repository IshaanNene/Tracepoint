package analysis

import (
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Telemetry sources, as they appear in result.telemetry.
const (
	SourceGenerator = "generator"
	SourcePostgres  = "postgres"
	SourceMySQL     = "mysql"
	SourceRedis     = "redis"
)

// runnerForSource maps a datastore sampler to the runner that probes the same tier.
// Generator telemetry corroborates no target tier; it speaks only about us.
func runnerForSource(source string) string {
	switch source {
	case SourcePostgres, SourceMySQL:
		return "db"
	case SourceRedis:
		return "redis"
	default:
		return ""
	}
}

// direction is which way a signal moves when something is wrong.
type direction int

const (
	up direction = iota
	down
)

// signalRule is one telemetry metric worth watching and the size of move that counts.
//
// A rise must clear both an absolute and a relative bar, for the same reason the
// relative hot-bucket rule has two guards: a counter moving from 0 to 1 is infinite in
// ratio and meaningless in practice. A fall must start from a baseline worth falling
// from.
type signalRule struct {
	source string
	key    string
	dir    direction
	// minAbs is the smallest absolute move that counts.
	minAbs float64
	// ratio: for up, the peak must be at least ratio times the baseline; for down,
	// the trough must be at most ratio times it.
	ratio float64
	// minBase is, for down, the smallest baseline a fall is judged from.
	minBase float64
	note    string
}

// rules is the fixed table of corroborating signals. Keys are documented per sampler
// in docs/METHODOLOGY.md. Order is the order signals are reported in.
//
// Throughput is deliberately absent. A tier's throughput falls when its server stalls,
// but it falls just as far when the application stops calling it because some other
// tier is stalled - a database lock starves the cache of requests too - so a fall in
// throughput cannot tell cause from victim. Every rule here moves only when the
// server itself is struggling.
//
//nolint:gochecknoglobals // A fixed table, never mutated.
var rules = []signalRule{
	{source: SourcePostgres, key: "locks_waiting", dir: up, minAbs: 1, ratio: 2,
		note: "sessions were waiting on locks"},
	{source: SourcePostgres, key: "sessions_waiting_lock", dir: up, minAbs: 1, ratio: 2,
		note: "backends reported a Lock wait event"},
	{source: SourcePostgres, key: "sessions_active", dir: up, minAbs: 3, ratio: 2,
		note: "active sessions piled up"},
	{source: SourcePostgres, key: "deadlocks", dir: up, minAbs: 1, ratio: 1,
		note: "the server detected deadlocks"},
	{source: SourcePostgres, key: "cache_hit_ratio", dir: down, minAbs: 0.05, ratio: 0.95, minBase: 0.5,
		note: "the buffer cache hit ratio fell, so reads went to disk"},
	{source: SourcePostgres, key: "temp_bytes", dir: up, minAbs: 1 << 20, ratio: 2,
		note: "queries spilled to temporary files"},
	{source: SourcePostgres, key: "sample_ms", dir: up, minAbs: 50, ratio: 3,
		note: "the sampler's own catalogue query slowed, so the server itself was struggling"},

	{source: SourceMySQL, key: "row_lock_waits", dir: up, minAbs: 1, ratio: 2,
		note: "InnoDB row lock waits rose"},
	{source: SourceMySQL, key: "threads_running", dir: up, minAbs: 3, ratio: 2,
		note: "running threads piled up"},
	{source: SourceMySQL, key: "buffer_pool_hit_ratio", dir: down, minAbs: 0.05, ratio: 0.95, minBase: 0.5,
		note: "the buffer pool hit ratio fell, so reads went to disk"},
	{source: SourceMySQL, key: "sample_ms", dir: up, minAbs: 50, ratio: 3,
		note: "the sampler's own status query slowed, so the server itself was struggling"},

	{source: SourceRedis, key: "blocked_clients", dir: up, minAbs: 1, ratio: 2,
		note: "clients were blocked"},
	{source: SourceRedis, key: "sample_ms", dir: up, minAbs: 50, ratio: 3,
		note: "the sampler's own INFO call stalled, so the server stopped answering"},
	{source: SourceRedis, key: "latency_max_ms", dir: up, minAbs: 50, ratio: 3,
		note: "the server's latency monitor recorded a spike"},
	{source: SourceRedis, key: "evicted_keys", dir: up, minAbs: 1, ratio: 2,
		note: "keys were evicted under memory pressure"},
	{source: SourceRedis, key: "rejected_connections", dir: up, minAbs: 1, ratio: 1,
		note: "the server rejected connections"},
	{source: SourceRedis, key: "hit_ratio", dir: down, minAbs: 0.1, ratio: 0.9, minBase: 0.3,
		note: "the cache hit ratio fell"},
}

// signalIndex holds the datastore samplers' series for looking up what moved in a
// window.
type signalIndex struct {
	bucketMS float64
	warmupMS float64
	series   map[string][]map[string]any
}

func newSignalIndex(t *result.Telemetry, bucketMS, warmupMS float64) *signalIndex {
	idx := &signalIndex{bucketMS: bucketMS, warmupMS: warmupMS, series: map[string][]map[string]any{}}
	if t == nil {
		return idx
	}
	add := func(name string, s *result.SamplerSeries) {
		if s != nil && s.Available && len(s.Samples) > 0 {
			idx.series[name] = s.Samples
		}
	}
	add(SourcePostgres, t.Postgres)
	add(SourceMySQL, t.MySQL)
	add(SourceRedis, t.Redis)
	return idx
}

// within returns every rule that fired inside the window, padded by one bucket either
// side because a sampler's interval need not line up with the buckets. The baseline is
// the median of that metric outside the padded window and after warm-up.
func (idx *signalIndex) within(s span) []result.TelemetrySignal {
	if idx == nil || len(idx.series) == 0 {
		return nil
	}
	lo := float64(s.start-1) * idx.bucketMS
	hi := float64(s.end+2) * idx.bucketMS
	var out []result.TelemetrySignal
	for _, rule := range rules {
		samples := idx.series[rule.source]
		if len(samples) == 0 {
			continue
		}
		var inside, outside []float64
		for _, smp := range samples {
			v, ok := number(smp[rule.key])
			if !ok {
				continue
			}
			t, _ := number(smp["t_ms"])
			switch {
			case t >= lo && t < hi:
				inside = append(inside, v)
			case t >= idx.warmupMS:
				outside = append(outside, v)
			}
		}
		if len(inside) == 0 || len(outside) == 0 {
			continue
		}
		base := median(outside)
		if sig, ok := rule.judge(inside, base); ok {
			out = append(out, sig)
		}
	}
	return out
}

func (r signalRule) judge(inside []float64, base float64) (result.TelemetrySignal, bool) {
	sig := result.TelemetrySignal{Source: r.source, Signal: r.key, Baseline: round3(base), Note: r.note}
	switch r.dir {
	case up:
		peak := inside[0]
		for _, v := range inside {
			if v > peak {
				peak = v
			}
		}
		sig.Value = round3(peak)
		return sig, peak-base >= r.minAbs && peak >= r.ratio*base
	default:
		trough := inside[0]
		for _, v := range inside {
			if v < trough {
				trough = v
			}
		}
		sig.Value = round3(trough)
		return sig, base >= r.minBase && base-trough >= r.minAbs && trough <= r.ratio*base
	}
}

// number reads a sample value, which is float64 after a JSON round trip and may be
// any numeric type when the result was built in memory.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	default:
		return 0, false
	}
}
