package telemetry

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/runner/redisrun"
)

// redisSampler samples INFO and, optionally, LATENCY LATEST.
//
// Keys:
//
//	ops_per_sec              commands processed per second over the interval
//	connected_clients, blocked_clients, used_memory_bytes   gauges
//	hit_ratio                keyspace hits / (hits + misses) over the interval
//	evicted_keys, rejected_connections   deltas since the previous sample
//	latency_max_ms           the worst latency-monitor event recorded since the
//	                         previous sample (only with telemetry.redis.latency)
//
// In cluster mode INFO reaches one node, so these describe that node.
type redisSampler struct {
	spec   Spec
	client redis.UniversalClient
	now    func() time.Time
	c      counters
	seen   map[string]int64 // latency event -> last timestamp reported
}

func newRedis(spec Spec) (Sampler, error) {
	if spec.Redis == nil {
		return nil, errs.New(errs.CodeConfigMissingField, "the redis sampler has nothing to connect to").
			WithPath("/telemetry/redis").
			WithHint("use it with a redis section, or set telemetry.redis.dsn to a redis:// URL")
	}
	return &redisSampler{spec: spec, now: spec.now(), seen: map[string]int64{}}, nil
}

// RedisFromDSN reads a sampler's own dsn: a redis:// or rediss:// URL, or a bare
// host:port.
func RedisFromDSN(dsn string) (*config.Redis, error) {
	if !strings.Contains(dsn, "://") {
		return &config.Redis{Addr: dsn}, nil
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalidValue, err, "telemetry.redis.dsn is not a redis URL").
			WithPath("/telemetry/redis/dsn")
	}
	out := &config.Redis{Addr: opt.Addr, Username: opt.Username, Password: opt.Password, DB: opt.DB}
	if opt.TLSConfig != nil {
		out.TLS = &config.TLS{ServerName: opt.TLSConfig.ServerName}
	}
	return out, nil
}

func (r *redisSampler) Name() string { return "redis" }

func (r *redisSampler) Open(ctx context.Context) error {
	opts, err := redisrun.ClientOptions(r.spec.Redis, "tracepoint-telemetry-"+r.spec.RunID, 1)
	if err != nil {
		return err
	}
	// One connection, and no retries: a sampler that quietly retried would hide the
	// very stall it exists to see.
	opts.MaxRetries = -1
	opts.ReadTimeout = sampleTimeout
	opts.WriteTimeout = sampleTimeout
	r.client = redis.NewUniversalClient(opts)
	if err := r.client.Ping(ctx).Err(); err != nil {
		_ = r.client.Close()
		r.client = nil
		return fmt.Errorf("connecting: %w", err)
	}
	if err := r.client.Info(ctx, "stats").Err(); err != nil {
		_ = r.client.Close()
		r.client = nil
		return errs.Wrap(errs.CodePreflightRedisPing, err, "INFO is not permitted").
			WithHint("allow the INFO command for this user (ACL +info)")
	}
	return nil
}

func (r *redisSampler) Sample(ctx context.Context, _ time.Duration) (Sample, error) {
	raw, err := r.client.Info(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("reading INFO: %w", err)
	}
	info := parseInfo(raw)

	s := Sample{}
	for key, field := range map[string]string{
		"connected_clients": "connected_clients",
		"blocked_clients":   "blocked_clients",
		"used_memory_bytes": "used_memory",
	} {
		if v, ok := info[field]; ok {
			s[key] = v
		}
	}
	d, elapsed, ok := r.c.step(r.now(), map[string]float64{
		"commands": info["total_commands_processed"],
		"hits":     info["keyspace_hits"], "misses": info["keyspace_misses"],
		"evicted_keys": info["evicted_keys"], "rejected_connections": info["rejected_connections"],
	})
	if ok {
		if secs := elapsed.Seconds(); secs > 0 {
			s["ops_per_sec"] = round3(d["commands"] / secs)
		}
		if h, has := ratio(d["hits"], d["hits"]+d["misses"]); has {
			s["hit_ratio"] = round3(h)
		}
		for _, k := range []string{"evicted_keys", "rejected_connections"} {
			if v, has := d[k]; has {
				s[k] = v
			}
		}
	}

	if r.spec.Latency {
		if worst, lerr := r.latency(ctx); lerr == nil {
			s["latency_max_ms"] = worst
		}
	}
	return s, nil
}

// latency reads LATENCY LATEST and returns the worst latency among events recorded
// since the previous sample. Each row is [event, unix-time, latest-ms, max-ms].
func (r *redisSampler) latency(ctx context.Context) (float64, error) {
	rows, err := r.client.Do(ctx, "LATENCY", "LATEST").Slice()
	if err != nil {
		return 0, fmt.Errorf("reading LATENCY LATEST: %w", err)
	}
	var worst float64
	for _, row := range rows {
		cols, ok := row.([]any)
		if !ok || len(cols) < 3 {
			continue
		}
		event, okEvent := cols[0].(string)
		at, okAt := cols[1].(int64)
		latest, okLatest := cols[2].(int64)
		if !okEvent || !okAt || !okLatest {
			continue
		}
		if at > r.seen[event] {
			r.seen[event] = at
			if float64(latest) > worst {
				worst = float64(latest)
			}
		}
	}
	return worst, nil
}

func (r *redisSampler) Close() error {
	if r.client == nil {
		return nil
	}
	if err := r.client.Close(); err != nil {
		return fmt.Errorf("closing the redis sampler: %w", err)
	}
	return nil
}

// parseInfo reads the numeric fields of an INFO reply.
func parseInfo(raw string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			out[k] = f
		}
	}
	return out
}
