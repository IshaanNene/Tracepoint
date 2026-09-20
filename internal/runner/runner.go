// Package runner defines the Runner interface, the iteration handed to it, and the
// registry that lets new protocols be added without touching the engine.
//
// A runner is the only part of the system that knows how to talk to a target. It is
// handed an iteration - which carries the timing stamps the executor has already
// taken - performs one operation, and records the outcome. Everything else about
// scheduling, bucketing and analysis happens around it.
package runner

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
)

// Runner drives one tier. Storage runners are simultaneously load and probe: because
// they hit the datastore directly, their latency is a health signal for that tier
// under the combined pressure of the application's traffic and this synthetic traffic.
type Runner interface {
	// Name is the runner's name: http, db or redis. It is also its key in result.json.
	Name() string

	// Kind says whether this is the tier users talk to or one behind it.
	Kind() metrics.Kind

	// Labels are the request, step, query or command names this runner will report,
	// known before the run starts so the record path never hashes a string.
	Labels() []string

	// SetStart fixes the run's monotonic origin, once, after preflight and before any
	// load. Every offset a runner records is measured from it.
	//
	// It is part of the interface rather than an optional extra on purpose: when it
	// was optional, two of three runners silently measured from the zero time and
	// reported latencies of several thousand years. A step that every implementation
	// must take belongs where the compiler checks for it.
	SetStart(t time.Time)

	// Prepare opens pools, resolves targets and runs preflight. It is called before
	// any load and may take as long as it needs; failures here cost nothing but time,
	// which is the whole point of doing them first.
	Prepare(ctx context.Context) error

	// Do performs exactly one operation and records its outcome. It returns an error
	// only when the run cannot continue; an operation that merely failed is recorded
	// as an error outcome and returns nil, because a failing target is a measurement,
	// not a malfunction.
	Do(ctx context.Context, it *Iteration, rec metrics.Recorder) error

	// Close releases pools and connections. It is called once, after the run.
	Close() error
}

// Iteration is one unit of work handed to a runner.
//
// The timing stamps are filled in by the executor before Do is called, and the runner
// copies them into the outcome it records along with the stamps only it can know.
// Intended in particular must be preserved: measuring from it rather than from
// Dispatched is what makes latency coordinated-omission correct.
type Iteration struct {
	// Index is the arrival's sequence number within the run.
	Index int64
	// Worker identifies the goroutine performing it.
	Worker int

	// Intended is when the schedule said this operation should be sent.
	Intended time.Duration
	// Dispatched is when the executor handed it to a worker.
	Dispatched time.Duration
	// WorkerStart is when the worker picked it up.
	WorkerStart time.Duration

	// Rand is this worker's own random source, so weighted picks and generators never
	// contend on a shared one. It is seeded from the run seed, so the sequence is
	// reproducible.
	Rand *rand.Rand
}

// StampOutcome copies the executor's timing stamps into an outcome. Runners call it
// first, then fill in ConnAcquired, FirstByte and End themselves.
func (it *Iteration) StampOutcome(o *metrics.Outcome) {
	o.Intended = it.Intended
	o.Dispatched = it.Dispatched
	o.WorkerStart = it.WorkerStart
}

// Factory builds a runner from its already-validated configuration. The cfg argument
// is the runner's own section of the configuration, passed as an opaque value so that
// this package does not depend on the config package.
type Factory func(cfg any, deps Deps) (Runner, error)

// Deps are what every runner needs from the engine. Everything here is injected
// rather than reached for: the clock so timings are testable, the start instant so
// every runner and sampler shares one timeline, and the seed so a run is reproducible.
type Deps struct {
	RunID     string
	Collector *metrics.Collector
	Clock     clock.Clock
	Start     time.Time
	Seed      uint64
	Logger    *slog.Logger
}

// Elapsed is the offset from the run start, which is the only time base anything in a
// result is expressed in.
func (d Deps) Elapsed() time.Duration { return d.Clock.Now().Sub(d.Start) }

//nolint:gochecknoglobals // A process-wide registry, guarded by its own mutex.
var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a runner factory under a name. It is called from package init
// functions, so a runner is available simply by being linked in - which is what lets
// a gRPC or Kafka runner be added later without the engine changing.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// New builds a registered runner.
func New(name string, cfg any, deps Deps) (Runner, error) {
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, errs.New(errs.CodeConfigInvalidValue, "no runner named %q", name).
			WithHint("available runners: %v", Registered())
	}
	return f(cfg, deps)
}

// Registered lists the runners compiled into this binary, sorted. The capabilities
// manifest is generated from it, so an agent can discover what this build supports
// rather than guessing.
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
