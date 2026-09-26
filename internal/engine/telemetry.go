package engine

import (
	"context"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/telemetry"
)

// DefaultTelemetryInterval is how often samplers read when nothing says otherwise.
const DefaultTelemetryInterval = time.Second

// openTelemetry builds and connects the generator's own sampler and every datastore
// sampler the configuration enables.
func (e *Engine) openTelemetry(ctx context.Context) (*telemetry.Loop, error) {
	cfg := e.opts.Config
	base := DefaultTelemetryInterval
	if cfg.Telemetry != nil && cfg.Telemetry.Interval != nil {
		base = cfg.Telemetry.Interval.D()
	}

	entries := []telemetry.Entry{{
		Sampler:  telemetry.NewGenerator(e.clk.Now, e.runnerHealth),
		Interval: base,
	}}
	for _, name := range cfg.Samplers() {
		spec, err := samplerSpec(cfg, name, e.opts.RunID, base)
		if err != nil {
			return nil, err
		}
		spec.Clock = e.clk
		s, err := telemetry.New(spec)
		if err != nil {
			return nil, err
		}
		entries = append(entries, telemetry.Entry{Sampler: s, Interval: spec.Interval, Timing: true})
	}
	return telemetry.Open(ctx, e.clk, e.log, entries), nil
}

// samplerSpec resolves what one datastore sampler connects to: its own dsn if it has
// one, otherwise the settings of the runner that loads the same tier.
func samplerSpec(cfg *config.Config, name, runID string, base time.Duration) (telemetry.Spec, error) {
	var s *config.Sampler
	switch name {
	case "postgres":
		s = cfg.Telemetry.Postgres
	case "mysql":
		s = cfg.Telemetry.MySQL
	case "redis":
		s = cfg.Telemetry.Redis
	}
	spec := telemetry.Spec{Name: name, RunID: runID, Interval: base}
	if s == nil {
		return spec, nil
	}
	if s.Interval != nil {
		spec.Interval = s.Interval.D()
	}
	spec.Statements, spec.Latency = s.Statements, s.Latency

	switch name {
	case "postgres", "mysql":
		spec.DSN = s.DSN
		if spec.DSN == "" && cfg.DB != nil && config.NormaliseDriver(cfg.DB.Driver) == name {
			spec.DSN = cfg.DB.DSN
		}
	case "redis":
		if s.DSN != "" {
			r, err := telemetry.RedisFromDSN(s.DSN)
			if err != nil {
				return spec, err
			}
			spec.Redis = r
		} else {
			spec.Redis = cfg.Redis
		}
	}
	return spec, nil
}

// runnerHealth is the per-runner view the generator sampler reports.
func (e *Engine) runnerHealth() map[string]telemetry.RunnerHealth {
	out := make(map[string]telemetry.RunnerHealth, len(e.runners))
	for _, br := range e.runners {
		ex := br.current()
		if ex == nil {
			continue
		}
		lag, ok := ex.RecentLag()
		out[br.name] = telemetry.RunnerHealth{InFlight: ex.InFlight(), DispatchLagP99MS: lag, HasLag: ok}
	}
	return out
}

// telemetryResult converts sampler output to the result document's shape.
func telemetryResult(series []telemetry.Series) *result.Telemetry {
	if len(series) == 0 {
		return nil
	}
	out := &result.Telemetry{}
	for _, s := range series {
		samples := make([]map[string]any, len(s.Samples))
		for i, smp := range s.Samples {
			samples[i] = smp
		}
		if s.Name == "generator" {
			out.Generator = &result.GeneratorSeries{Samples: samples}
			continue
		}
		rs := &result.SamplerSeries{
			Available: s.Available, Reason: s.Reason,
			IntervalMS: float64(s.Interval) / float64(time.Millisecond),
			Samples:    samples, Statements: s.Statements,
		}
		switch s.Name {
		case "postgres":
			out.Postgres = rs
		case "mysql":
			out.MySQL = rs
		case "redis":
			out.Redis = rs
		}
	}
	return out
}
