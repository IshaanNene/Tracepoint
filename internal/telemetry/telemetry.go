// Package telemetry samples server-side health from the datastores, and the generator's
// own health, on the shared offset clock.
//
// Samplers are opt-in (the generator's is always on), read-only, use one dedicated
// connection each, and fail soft: a sampler that cannot connect or lacks a grant
// reports itself unavailable with a reason, and the run carries on with latency
// evidence alone. Telemetry corroborates a verdict; it must never be the reason a run
// fails.
//
// Samplers live in a registry so that a new datastore plugs in without the engine
// changing - the same extension point runners have (§3.2).
package telemetry

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Sample is one reading. Keys are documented per sampler in docs/METHODOLOGY.md; the
// loop adds t_ms (the offset the sample was taken at) and sample_ms (how long taking
// it took, which is itself a signal: a server that stalls stalls its own sampler).
type Sample map[string]any

// Sampler reads one source of health.
type Sampler interface {
	Name() string
	// Sample takes a reading at offset at from the run start. Counters should be
	// reported as deltas from the previous call, so readers need not difference them.
	Sample(ctx context.Context, at time.Duration) (Sample, error)
}

// Opener is implemented by samplers that need a connection. An error here marks the
// sampler unavailable for the whole run, with the error as its reason.
type Opener interface {
	Open(ctx context.Context) error
}

// Closer releases a sampler's connection.
type Closer interface {
	Close() error
}

// Finisher is implemented by samplers with an end-of-run snapshot, such as Postgres's
// top statements.
type Finisher interface {
	Finish(ctx context.Context) ([]map[string]any, error)
}

// Limiter is implemented by samplers that can run with reduced visibility - a missing
// grant, say - and want to say so without being marked unavailable.
type Limiter interface {
	Limitation() string
}

// Spec is what a datastore sampler is built from.
type Spec struct {
	// Name is the sampler: postgres, mysql or redis.
	Name string
	// DSN is the connection string for SQL samplers.
	DSN string
	// Redis is the Redis deployment to sample, for the redis sampler.
	Redis *config.Redis
	// RunID tags the sampler's own session so it can be told apart from the load.
	RunID string
	// Statements asks Postgres for an end-of-run pg_stat_statements snapshot.
	Statements bool
	// Latency asks Redis for LATENCY LATEST on every sample.
	Latency bool
	// Interval is how often to sample.
	Interval time.Duration
	// Clock times the intervals that per-second rates are computed over. Defaults to
	// the real clock.
	Clock clock.Clock
}

func (s Spec) now() func() time.Time {
	if s.Clock == nil {
		return time.Now
	}
	return s.Clock.Now
}

// Factory builds a sampler from its spec. It must not touch the network; Open does.
type Factory func(Spec) (Sampler, error)

//nolint:gochecknoglobals // A process-wide registry, guarded by its own mutex.
var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a sampler factory under a name.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// New builds a registered sampler.
func New(spec Spec) (Sampler, error) {
	registryMu.RLock()
	f, ok := registry[spec.Name]
	registryMu.RUnlock()
	if !ok {
		return nil, errs.New(errs.CodeConfigInvalidValue, "no telemetry sampler named %q", spec.Name).
			WithHint("available samplers: %v", Registered())
	}
	return f(spec)
}

// Registered lists the samplers compiled into this binary, sorted.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func init() {
	Register("postgres", newPostgres)
	Register("mysql", newMySQL)
	Register("redis", newRedis)
}

// ms converts a duration to fractional milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// round3 keeps three decimals, so the same reading always serialises to the same
// digits.
func round3(v float64) float64 {
	if v < 0 {
		return -round3(-v)
	}
	return float64(int64(v*1000+0.5)) / 1000
}
