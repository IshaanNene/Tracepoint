// Package redisrun drives Redis load and, at the same time, probes the cache.
//
// Like the SQL runner this is load and probe at once: it talks to Redis directly, so
// its latency measures that tier's health while the application is under pressure.
//
// Redis is single-threaded per shard, which makes it unusually informative as a probe:
// when one client blocks the server, every client sees it. A probe going hot while the
// application goes hot is therefore strong evidence, and a probe staying flat while the
// application struggles is strong evidence the other way.
package redisrun

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/template"
)

// Name is how this runner is registered and how it appears in result.json.
const Name = "redis"

// Runner drives Redis load.
type Runner struct {
	cfg    *config.Redis
	deps   runner.Deps
	client redis.UniversalClient

	commands []*command
	picker   *runner.Weighted
	shared   *template.Shared
	timeout  time.Duration

	reads, writes int64
}

// command is one compiled, weighted unit of work: a single command or a pipeline.
type command struct {
	name       string
	label      metrics.LabelID
	op         policy.Op
	timeout    time.Duration
	isPipeline bool
	// parts holds the argument templates for each command in the unit.
	parts [][]*template.Template
	// static holds fully resolved arguments when nothing needs rendering.
	static [][]any
}

// New builds a Redis runner from an already-validated configuration.
func New(cfg *config.Redis, defaultTimeout time.Duration, deps runner.Deps) (*Runner, error) {
	if cfg == nil {
		return nil, errs.New(errs.CodeInternal, "the redis runner needs a configuration")
	}
	r := &Runner{
		cfg: cfg, deps: deps,
		shared: template.NewShared(deps.Clock), timeout: defaultTimeout,
	}

	weights := make([]float64, 0, len(cfg.Commands))
	for i := range cfg.Commands {
		c, err := r.compile(&cfg.Commands[i])
		if err != nil {
			return nil, err
		}
		r.commands = append(r.commands, c)
		weights = append(weights, cfg.Commands[i].WeightOr())
	}
	r.picker = runner.NewWeighted(weights)

	opts, err := r.clientOptions()
	if err != nil {
		return nil, err
	}
	r.client = redis.NewUniversalClient(opts)
	return r, nil
}

func (r *Runner) compile(src *config.Command) (*command, error) {
	c := &command{
		name:       src.Name,
		label:      r.deps.Collector.LabelID(src.Name),
		timeout:    r.timeout,
		isPipeline: len(src.Pipeline) > 0,
	}
	if src.Timeout != nil {
		c.timeout = src.Timeout.D()
	}

	units := make([][]string, 0, len(src.Pipeline)+1)
	if len(src.Cmd) > 0 {
		units = append(units, src.Cmd)
	}
	units = append(units, src.Pipeline...)

	// The declared type wins; otherwise the classifier decides, and anything it does
	// not recognise counts as a write.
	c.op = policy.Op(src.Type)
	inferred := policy.OpRead
	for _, u := range units {
		if policy.ClassifyRedis(u).Op == policy.OpWrite {
			inferred = policy.OpWrite
		}
	}
	if c.op == "" {
		c.op = inferred
	}

	allStatic := true
	for _, unit := range units {
		templates := make([]*template.Template, 0, len(unit))
		for i, arg := range unit {
			tpl, err := template.Compile(arg)
			if err != nil {
				var typed *errs.Error
				if errors.As(err, &typed) {
					return nil, typed.WithPath("/redis/commands").
						WithDetail("command", src.Name).
						WithDetail("argument", i)
				}
				return nil, err
			}
			if !tpl.IsStatic() {
				allStatic = false
			}
			templates = append(templates, tpl)
		}
		c.parts = append(c.parts, templates)
	}

	// A command whose arguments never change is resolved once here, so the hot path
	// does no work at all for the common case of a fixed PING or a fixed key.
	if allStatic {
		for _, unit := range c.parts {
			args := make([]any, 0, len(unit))
			for _, tpl := range unit {
				args = append(args, tpl.Static())
			}
			c.static = append(c.static, args)
		}
	}
	return c, nil
}

func (r *Runner) clientOptions() (*redis.UniversalOptions, error) {
	addrs := r.cfg.Addrs
	if len(addrs) == 0 && r.cfg.Addr != "" {
		addrs = []string{r.cfg.Addr}
	}

	workers := r.cfg.Executor.MaxInFlight
	if workers <= 0 {
		workers = 16
	}
	poolSize, minIdle := workers, 0
	if p := r.cfg.Pool; p != nil {
		if p.Size > 0 {
			poolSize = p.Size
		}
		minIdle = p.MinIdle
	}

	opts := &redis.UniversalOptions{
		Addrs:        addrs,
		Username:     r.cfg.Username,
		Password:     r.cfg.Password,
		DB:           r.cfg.DB,
		PoolSize:     poolSize,
		MinIdleConns: minIdle,
		// The client name shows up in CLIENT LIST, so an operator can see which
		// connections belong to the test.
		ClientName: "tracepoint-" + r.deps.RunID,
	}
	switch r.cfg.Mode {
	case "sentinel":
		opts.MasterName = r.cfg.MasterName
	case "cluster":
		// UniversalClient picks cluster mode from having several addresses, but a
		// single-node cluster is legitimate and must not silently become a plain client.
		opts.RouteRandomly = true
	default:
	}

	if r.cfg.TLS != nil {
		cfg, err := buildTLS(r.cfg.TLS)
		if err != nil {
			return nil, err
		}
		opts.TLSConfig = cfg
	}
	return opts, nil
}

func buildTLS(c *config.TLS) (*tls.Config, error) {
	//nolint:gosec // InsecureSkipVerify is opt-in, gated by policy, and printed in every report.
	out := &tls.Config{
		InsecureSkipVerify: c.InsecureSkipVerify,
		ServerName:         c.ServerName,
		MinVersion:         tls.VersionTLS12,
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, errs.Wrap(errs.CodeIOReadFailed, err, "reading the CA bundle %s", c.CAFile)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errs.New(errs.CodeConfigInvalidValue, "%s contains no usable certificates", c.CAFile)
		}
		out.RootCAs = pool
	}
	if c.CertFile != "" || c.KeyFile != "" {
		pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, errs.Wrap(errs.CodeConfigInvalidValue, err, "loading the redis client certificate")
		}
		out.Certificates = []tls.Certificate{pair}
	}
	return out, nil
}

// SetStart fixes the run's monotonic origin. The engine calls it once, after preflight
// and before any load, because every offset this runner records is measured from it.
func (r *Runner) SetStart(t time.Time) { r.deps.Start = t }

// Name identifies the runner.
func (r *Runner) Name() string { return Name }

// Kind reports that this is a tier behind the application.
func (r *Runner) Kind() metrics.Kind { return metrics.KindStorage }

// Labels are the command names.
func (r *Runner) Labels() []string {
	out := make([]string, 0, len(r.commands))
	for _, c := range r.commands {
		out = append(out, c.name)
	}
	return out
}

// Counts reports how many operations were reads and how many were writes.
func (r *Runner) Counts() (reads, writes int64) { return r.reads, r.writes }

// PoolStats reports what the connection pool did. Timeouts here are generator-side.
func (r *Runner) PoolStats() *redis.PoolStats { return r.client.PoolStats() }

// Prepare checks that the server answers before any load starts.
func (r *Runner) Prepare(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return errs.Wrap(errs.CodePreflightRedisPing, err, "redis did not answer").
			WithHint("check the address, and that any password and TLS settings are right")
	}

	// A pool smaller than the concurrency turns every operation into a queue and
	// reports the wait as Redis latency, which is exactly the confusion this tool
	// exists to prevent. Worth saying before the run rather than in the analysis.
	workers := r.cfg.Executor.MaxInFlight
	if stats := r.client.PoolStats(); workers > 0 && stats != nil {
		if r.cfg.Pool != nil && r.cfg.Pool.Size > 0 && r.cfg.Pool.Size < workers {
			r.deps.Logger.Warn("the redis pool is smaller than the worker count, so operations will queue inside the client",
				"pool_size", r.cfg.Pool.Size, "workers", workers)
		}
	}
	return nil
}

// Do performs one command or pipeline and records its outcome.
func (r *Runner) Do(ctx context.Context, it *runner.Iteration, rec metrics.Recorder) error {
	c := r.commands[r.picker.Pick(it.Rand)]

	var o metrics.Outcome
	it.StampOutcome(&o)
	o.Label = c.label

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	args, err := r.render(c, it)
	if err != nil {
		// A template that cannot be rendered is not the target's fault, and the
		// command must not be sent with a literal token in it.
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		o.Class = metrics.ClassExtractFailed
		rec.Record(&o)
		return nil
	}

	// go-redis owns its pool, so the closest honest mark for "a connection was in
	// hand" is the moment the command is handed to the client. Any pool wait inside it
	// is reported through PoolStats rather than being silently added to service time.
	o.ConnAcquired = r.deps.Elapsed()
	execErr := r.execute(ctx, c, args)
	o.End = r.deps.Elapsed()

	if execErr != nil {
		o.Class = classifyError(ctx, execErr)
	} else {
		o.Class = metrics.ClassOK
	}
	if c.op == policy.OpWrite {
		r.writes++
	} else {
		r.reads++
	}
	rec.Record(&o)
	return nil
}

func (r *Runner) render(c *command, it *runner.Iteration) ([][]any, error) {
	if c.static != nil {
		return c.static, nil
	}
	tctx := &template.Context{Shared: r.shared, Rand: it.Rand}
	out := make([][]any, 0, len(c.parts))
	for _, unit := range c.parts {
		args := make([]any, 0, len(unit))
		for _, tpl := range unit {
			v, err := tpl.RenderValue(tctx)
			if err != nil {
				return nil, err
			}
			args = append(args, v)
		}
		out = append(out, args)
	}
	return out, nil
}

func (r *Runner) execute(ctx context.Context, c *command, units [][]any) error {
	if !c.isPipeline {
		// redis.Nil means the key was absent, which is a perfectly ordinary answer to
		// a GET and not a failure of anything.
		if err := r.client.Do(ctx, units[0]...).Err(); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("running %v: %w", units[0][0], err)
		}
		return nil
	}

	pipe := r.client.Pipeline()
	for _, args := range units {
		pipe.Do(ctx, args...)
	}
	cmds, err := pipe.Exec(ctx)
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("running a pipeline: %w", err)
	}
	for _, cmd := range cmds {
		if cmdErr := cmd.Err(); cmdErr != nil && !errors.Is(cmdErr, redis.Nil) {
			return fmt.Errorf("in a pipeline: %w", cmdErr)
		}
	}
	return nil
}

// classifyError sorts a failure into the taxonomy the abort guard and the validity
// rules read. The distinction that matters is between the server being unreachable and
// the server answering with a refusal.
func classifyError(ctx context.Context, err error) metrics.Class {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return metrics.ClassTimeout
	case errors.Is(err, context.Canceled):
		return metrics.ClassCanceled
	case errors.Is(err, redis.ErrClosed):
		return metrics.ClassConnection
	}
	if ctx.Err() != nil {
		return metrics.ClassCanceled
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "eof"),
		strings.Contains(msg, "no such host"):
		return metrics.ClassConnection
	case strings.Contains(msg, "pool timeout"), strings.Contains(msg, "connection pool"):
		// Our own pool ran out. A generator-side limit, not a Redis problem.
		return metrics.ClassPoolTimeout
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "timeout"):
		return metrics.ClassTimeout
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return metrics.ClassTLS
	default:
		// The server answered and said no: a wrong type, a missing argument, an ACL
		// refusal. That is about the command, not about Redis's health.
		return metrics.ClassQueryError
	}
}

// Close releases the pool.
func (r *Runner) Close() error {
	if err := r.client.Close(); err != nil {
		return fmt.Errorf("closing the redis client: %w", err)
	}
	return nil
}

var _ runner.Runner = (*Runner)(nil)
