// Package config loads, interpolates, strictly decodes, defaults and validates a
// TracePoint configuration.
//
// Unknown keys are errors, not warnings, and they carry the line, the column and a
// suggestion - a configuration that silently ignores a misspelled field wastes a whole
// test run before anyone notices. The struct shapes here mirror
// schemas/config.schema.json exactly; a test round-trips the schema's own fixtures
// through them so the two cannot drift.
package config

import (
	"encoding/json"
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Version is the only configuration format version that exists.
const Version = 1

// Duration is a Go duration that decodes from a string such as "250ms" or "3m".
type Duration time.Duration

// UnmarshalYAML decodes a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("a duration must be a string such as \"250ms\" or \"3m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration; use a number and a unit such as 250ms, 30s, 3m or 1h30m", s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes a duration back as its string form, so the effective
// configuration stored with a run reads the way it was written.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// MarshalJSON writes a duration as its string form.
func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

// D converts to a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration the way it would be written in a configuration.
func (d Duration) String() string { return time.Duration(d).String() }

// Config is a whole TracePoint configuration.
type Config struct {
	Version   int        `yaml:"version" json:"version"`
	Run       Run        `yaml:"run" json:"run"`
	SLO       SLO        `yaml:"slo" json:"slo,omitzero"`
	Safety    Safety     `yaml:"safety" json:"safety,omitzero"`
	HTTP      *HTTP      `yaml:"http" json:"http,omitempty"`
	DB        *DB        `yaml:"db" json:"db,omitempty"`
	Redis     *Redis     `yaml:"redis" json:"redis,omitempty"`
	Telemetry *Telemetry `yaml:"telemetry" json:"telemetry,omitempty"`
	Capacity  *Capacity  `yaml:"capacity" json:"capacity,omitempty"`
}

// Run holds timing and determinism for the whole run.
type Run struct {
	Name       string            `yaml:"name" json:"name,omitempty"`
	Duration   Duration          `yaml:"duration" json:"duration"`
	Bucket     Duration          `yaml:"bucket" json:"bucket"`
	Warmup     Duration          `yaml:"warmup" json:"warmup"`
	Grace      Duration          `yaml:"grace" json:"grace"`
	Timeout    Duration          `yaml:"timeout" json:"timeout"`
	Seed       *uint64           `yaml:"seed" json:"seed,omitempty"`
	Arrival    string            `yaml:"arrival" json:"arrival"`
	MinSamples int               `yaml:"min_samples" json:"min_samples"`
	Tags       map[string]string `yaml:"tags" json:"tags,omitempty"`
}

// SLOTarget is one runner's latency and error budget, judged on response time.
type SLOTarget struct {
	P95       *Duration `yaml:"p95" json:"p95,omitempty"`
	P99       *Duration `yaml:"p99" json:"p99,omitempty"`
	ErrorRate *float64  `yaml:"error_rate" json:"error_rate,omitempty"`
}

// IsZero reports whether no budget was configured.
func (s SLOTarget) IsZero() bool { return s.P95 == nil && s.P99 == nil && s.ErrorRate == nil }

// SLO holds per-runner budgets.
type SLO struct {
	HTTP  SLOTarget `yaml:"http" json:"http,omitzero"`
	DB    SLOTarget `yaml:"db" json:"db,omitzero"`
	Redis SLOTarget `yaml:"redis" json:"redis,omitzero"`
}

// IsZero reports whether no budgets were configured at all.
func (s SLO) IsZero() bool { return s.HTTP.IsZero() && s.DB.IsZero() && s.Redis.IsZero() }

// For returns the budget for a runner by name.
func (s SLO) For(runner string) SLOTarget {
	switch runner {
	case "http":
		return s.HTTP
	case "db":
		return s.DB
	case "redis":
		return s.Redis
	default:
		return SLOTarget{}
	}
}

// Safety is what this configuration accepts. It can only tighten the human-granted
// policy, never widen it.
type Safety struct {
	AllowWrites      bool        `yaml:"allow_writes" json:"allow_writes"`
	AllowDangerous   bool        `yaml:"allow_dangerous" json:"allow_dangerous"`
	AllowTargets     []string    `yaml:"allow_targets" json:"allow_targets,omitempty"`
	AllowInsecureTLS bool        `yaml:"allow_insecure_tls" json:"allow_insecure_tls"`
	MaxRatePerRunner *float64    `yaml:"max_rate_per_runner" json:"max_rate_per_runner,omitempty"`
	MaxInFlight      *int        `yaml:"max_in_flight" json:"max_in_flight,omitempty"`
	MaxDuration      *Duration   `yaml:"max_duration" json:"max_duration,omitempty"`
	AbortGuard       *AbortGuard `yaml:"abort_guard" json:"abort_guard,omitempty"`
}

// IsZero reports whether nothing was tightened.
func (s Safety) IsZero() bool {
	return !s.AllowWrites && !s.AllowDangerous && len(s.AllowTargets) == 0 && !s.AllowInsecureTLS &&
		s.MaxRatePerRunner == nil && s.MaxInFlight == nil && s.MaxDuration == nil && s.AbortGuard == nil
}

// AbortGuard stops a run early when the target is clearly failing.
type AbortGuard struct {
	Enabled    *bool     `yaml:"enabled" json:"enabled,omitempty"`
	ErrorRatio *float64  `yaml:"error_ratio" json:"error_ratio,omitempty"`
	Window     *Duration `yaml:"window" json:"window,omitempty"`
}

// Stage is one segment of a load profile.
type Stage struct {
	Duration Duration `yaml:"duration" json:"duration"`
	Target   float64  `yaml:"target" json:"target"`
}

// Executor says how work is generated.
type Executor struct {
	Type        string  `yaml:"type" json:"type"`
	Rate        float64 `yaml:"rate" json:"rate,omitempty"`
	VUs         int     `yaml:"vus" json:"vus,omitempty"`
	Stages      []Stage `yaml:"stages" json:"stages,omitempty"`
	MaxInFlight int     `yaml:"max_in_flight" json:"max_in_flight,omitempty"`
	QueueDepth  *int    `yaml:"queue_depth" json:"queue_depth,omitempty"`
	Pick        string  `yaml:"pick" json:"pick,omitempty"`
}

// Expect is what a successful response looks like.
type Expect struct {
	Status       []int        `yaml:"status" json:"status,omitempty"`
	JSON         []JSONExpect `yaml:"json" json:"json,omitempty"`
	MaxBodyBytes int64        `yaml:"max_body_bytes" json:"max_body_bytes,omitempty"`
}

// JSONExpect is one assertion against a JSON response body.
type JSONExpect struct {
	Path   string `yaml:"path" json:"path"`
	Exists *bool  `yaml:"exists" json:"exists,omitempty"`
	Equals any    `yaml:"equals" json:"equals,omitempty"`
}

// Extract pulls a value out of a response for later steps in the same journey.
type Extract struct {
	Name string `yaml:"name" json:"name"`
	From string `yaml:"from" json:"from,omitempty"`
	Path string `yaml:"path" json:"path"`
}

// ThinkTime is a pause after a step. It is excluded from every latency statistic.
type ThinkTime struct {
	Type     string    `yaml:"type" json:"type"`
	Duration *Duration `yaml:"duration" json:"duration,omitempty"`
	Min      *Duration `yaml:"min" json:"min,omitempty"`
	Max      *Duration `yaml:"max" json:"max,omitempty"`
	Mean     *Duration `yaml:"mean" json:"mean,omitempty"`
}

// HTTPStep is one HTTP call. A top-level request is a single-step journey.
type HTTPStep struct {
	Name     string            `yaml:"name" json:"name"`
	Weight   *float64          `yaml:"weight" json:"weight,omitempty"`
	Method   string            `yaml:"method" json:"method,omitempty"`
	URL      string            `yaml:"url" json:"url"`
	Headers  map[string]string `yaml:"headers" json:"headers,omitempty"`
	Body     string            `yaml:"body" json:"body,omitempty"`
	BodyFile string            `yaml:"body_file" json:"body_file,omitempty"`
	Timeout  *Duration         `yaml:"timeout" json:"timeout,omitempty"`
	Expect   *Expect           `yaml:"expect" json:"expect,omitempty"`
	Extract  []Extract         `yaml:"extract" json:"extract,omitempty"`
	Think    *ThinkTime        `yaml:"think" json:"think,omitempty"`
}

// Journey is a weighted multi-step user journey.
type Journey struct {
	Name   string     `yaml:"name" json:"name"`
	Weight *float64   `yaml:"weight" json:"weight,omitempty"`
	Steps  []HTTPStep `yaml:"steps" json:"steps"`
}

// Feeder is a CSV file supplying parameter data.
type Feeder struct {
	Name string `yaml:"name" json:"name"`
	File string `yaml:"file" json:"file"`
	Mode string `yaml:"mode" json:"mode,omitempty"`
}

// TLS holds transport security settings.
type TLS struct {
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify" json:"insecure_skip_verify,omitempty"`
	ServerName         string `yaml:"server_name" json:"server_name,omitempty"`
	CAFile             string `yaml:"ca_file" json:"ca_file,omitempty"`
	CertFile           string `yaml:"cert_file" json:"cert_file,omitempty"`
	KeyFile            string `yaml:"key_file" json:"key_file,omitempty"`
}

// Transport tunes the HTTP client so that it is not itself the bottleneck.
type Transport struct {
	MaxIdleConnsPerHost int   `yaml:"max_idle_conns_per_host" json:"max_idle_conns_per_host,omitempty"`
	HTTP2               *bool `yaml:"http2" json:"http2,omitempty"`
	KeepAlive           *bool `yaml:"keep_alive" json:"keep_alive,omitempty"`
	TLS                 *TLS  `yaml:"tls" json:"tls,omitempty"`
}

// HTTP is the application tier: the traffic whose latency users feel.
type HTTP struct {
	BaseURL     string            `yaml:"base_url" json:"base_url,omitempty"`
	Headers     map[string]string `yaml:"headers" json:"headers,omitempty"`
	Traceparent bool              `yaml:"traceparent" json:"traceparent,omitempty"`
	Cookies     *bool             `yaml:"cookies" json:"cookies,omitempty"`
	Executor    Executor          `yaml:"executor" json:"executor"`
	Transport   *Transport        `yaml:"transport" json:"transport,omitempty"`
	Feeders     []Feeder          `yaml:"feeders" json:"feeders,omitempty"`
	Requests    []HTTPStep        `yaml:"requests" json:"requests,omitempty"`
	Journeys    []Journey         `yaml:"journeys" json:"journeys,omitempty"`
}

// Pool configures a SQL connection pool.
type Pool struct {
	MaxOpen         int       `yaml:"max_open" json:"max_open,omitempty"`
	MaxIdle         int       `yaml:"max_idle" json:"max_idle,omitempty"`
	ConnMaxLifetime *Duration `yaml:"conn_max_lifetime" json:"conn_max_lifetime,omitempty"`
	ConnMaxIdleTime *Duration `yaml:"conn_max_idle_time" json:"conn_max_idle_time,omitempty"`
}

// Arg is one bind argument.
//
// Written as a plain scalar in the ordinary case, or as {value: ..., secret: true}
// when it carries something that must never appear in a report. §8 requires that
// marker: a bind argument can hold an API key just as easily as a header can, and an
// artifact gets attached to a ticket long after anyone remembers what was in it.
type Arg struct {
	Value  any
	Secret bool
}

// UnmarshalYAML accepts both forms.
func (a *Arg) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		// Only a mapping that actually carries a `value` key is the object form; a
		// mapping without one is a JSON argument being passed through.
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != "value" {
				continue
			}
			var wrapper struct {
				Value  any  `yaml:"value"`
				Secret bool `yaml:"secret"`
			}
			if err := n.Decode(&wrapper); err != nil {
				return fmt.Errorf("decoding a bind argument: %w", err)
			}
			a.Value, a.Secret = wrapper.Value, wrapper.Secret
			return nil
		}
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return fmt.Errorf("decoding a bind argument: %w", err)
	}
	a.Value = v
	return nil
}

// MarshalYAML writes a plain argument back as a scalar and a secret one as the object
// form, so a round trip through the effective configuration preserves the marker.
func (a Arg) MarshalYAML() (any, error) {
	if a.Secret {
		return map[string]any{"value": a.Value, "secret": true}, nil
	}
	return a.Value, nil
}

// MarshalJSON mirrors MarshalYAML.
func (a Arg) MarshalJSON() ([]byte, error) {
	if a.Secret {
		b, err := json.Marshal(map[string]any{"value": a.Value, "secret": true})
		if err != nil {
			return nil, fmt.Errorf("encoding a secret bind argument: %w", err)
		}
		return b, nil
	}
	b, err := json.Marshal(a.Value)
	if err != nil {
		return nil, fmt.Errorf("encoding a bind argument: %w", err)
	}
	return b, nil
}

// String renders the argument for a plan or a log line, never revealing a secret.
func (a Arg) String() string {
	if a.Secret {
		return Redacted
	}
	return fmt.Sprint(a.Value)
}

// TxStatement is one statement inside a transaction unit.
type TxStatement struct {
	SQL  string `yaml:"sql" json:"sql"`
	Args []Arg  `yaml:"args" json:"args,omitempty"`
}

// Query is one weighted SQL statement or transaction unit.
type Query struct {
	Name    string        `yaml:"name" json:"name"`
	Weight  *float64      `yaml:"weight" json:"weight,omitempty"`
	Type    string        `yaml:"type" json:"type,omitempty"`
	SQL     string        `yaml:"sql" json:"sql,omitempty"`
	Args    []Arg         `yaml:"args" json:"args,omitempty"`
	Tx      []TxStatement `yaml:"tx" json:"tx,omitempty"`
	Timeout *Duration     `yaml:"timeout" json:"timeout,omitempty"`
	Prepare bool          `yaml:"prepare" json:"prepare,omitempty"`
}

// DB is the SQL tier: load and probe at once.
type DB struct {
	Driver         string   `yaml:"driver" json:"driver"`
	DSN            string   `yaml:"dsn" json:"dsn"`
	Executor       Executor `yaml:"executor" json:"executor"`
	Pool           *Pool    `yaml:"pool" json:"pool,omitempty"`
	TagSessions    *bool    `yaml:"tag_sessions" json:"tag_sessions,omitempty"`
	CommentQueries bool     `yaml:"comment_queries" json:"comment_queries,omitempty"`
	Queries        []Query  `yaml:"queries" json:"queries"`
}

// RedisPool configures the Redis client pool.
type RedisPool struct {
	Size    int `yaml:"size" json:"size,omitempty"`
	MinIdle int `yaml:"min_idle" json:"min_idle,omitempty"`
}

// Command is one weighted Redis command or pipeline.
type Command struct {
	Name     string     `yaml:"name" json:"name"`
	Weight   *float64   `yaml:"weight" json:"weight,omitempty"`
	Type     string     `yaml:"type" json:"type,omitempty"`
	Cmd      []string   `yaml:"cmd" json:"cmd,omitempty"`
	Pipeline [][]string `yaml:"pipeline" json:"pipeline,omitempty"`
	Timeout  *Duration  `yaml:"timeout" json:"timeout,omitempty"`
}

// Redis is the cache tier: load and probe at once.
type Redis struct {
	Mode       string     `yaml:"mode" json:"mode,omitempty"`
	Addr       string     `yaml:"addr" json:"addr,omitempty"`
	Addrs      []string   `yaml:"addrs" json:"addrs,omitempty"`
	MasterName string     `yaml:"master_name" json:"master_name,omitempty"`
	Username   string     `yaml:"username" json:"username,omitempty"`
	Password   string     `yaml:"password" json:"password,omitempty"`
	DB         int        `yaml:"db" json:"db,omitempty"`
	TLS        *TLS       `yaml:"tls" json:"tls,omitempty"`
	Executor   Executor   `yaml:"executor" json:"executor"`
	Pool       *RedisPool `yaml:"pool" json:"pool,omitempty"`
	Commands   []Command  `yaml:"commands" json:"commands"`
}

// Sampler is a telemetry sampler toggle, written either as a boolean or as an object.
type Sampler struct {
	Enabled    bool      `yaml:"enabled" json:"enabled"`
	Interval   *Duration `yaml:"interval" json:"interval,omitempty"`
	DSN        string    `yaml:"dsn" json:"dsn,omitempty"`
	Statements bool      `yaml:"statements" json:"statements,omitempty"`
	Latency    bool      `yaml:"latency" json:"latency,omitempty"`
}

// UnmarshalYAML accepts both forms the schema allows: `postgres: true` for defaults,
// or an object when something needs configuring.
func (s *Sampler) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var b bool
		if err := n.Decode(&b); err != nil {
			return fmt.Errorf("a sampler must be true, false, or an object: %w", err)
		}
		s.Enabled = b
		return nil
	}
	type plain Sampler // avoid recursing into this method
	v := plain{Enabled: true}
	if err := n.Decode(&v); err != nil {
		return fmt.Errorf("decoding a sampler: %w", err)
	}
	*s = Sampler(v)
	return nil
}

// Telemetry configures the server-side samplers.
type Telemetry struct {
	Interval *Duration `yaml:"interval" json:"interval,omitempty"`
	Postgres *Sampler  `yaml:"postgres" json:"postgres,omitempty"`
	MySQL    *Sampler  `yaml:"mysql" json:"mysql,omitempty"`
	Redis    *Sampler  `yaml:"redis" json:"redis,omitempty"`
}

// Capacity turns the run into an auto-ramp search.
type Capacity struct {
	Knob         string    `yaml:"knob" json:"knob"`
	Runner       string    `yaml:"runner" json:"runner,omitempty"`
	Start        float64   `yaml:"start" json:"start,omitempty"`
	Max          float64   `yaml:"max" json:"max,omitempty"`
	StepDuration *Duration `yaml:"step_duration" json:"step_duration,omitempty"`
	Settle       *Duration `yaml:"settle" json:"settle,omitempty"`
	Cooldown     *Duration `yaml:"cooldown" json:"cooldown,omitempty"`
	Refine       string    `yaml:"refine" json:"refine,omitempty"`
	Resolution   float64   `yaml:"resolution" json:"resolution,omitempty"`
	Confirm      *bool     `yaml:"confirm" json:"confirm,omitempty"`
}

// Runners lists the runner sections this configuration actually enables, in a stable
// order.
func (c *Config) Runners() []string {
	var out []string
	if c.HTTP != nil {
		out = append(out, "http")
	}
	if c.DB != nil {
		out = append(out, "db")
	}
	if c.Redis != nil {
		out = append(out, "redis")
	}
	return out
}

// Samplers lists the datastore telemetry samplers this configuration enables, in a
// stable order. The generator's own sampler is always on and is not listed.
func (c *Config) Samplers() []string {
	if c.Telemetry == nil {
		return nil
	}
	var out []string
	for _, s := range []struct {
		name string
		s    *Sampler
	}{{"postgres", c.Telemetry.Postgres}, {"mysql", c.Telemetry.MySQL}, {"redis", c.Telemetry.Redis}} {
		if s.s != nil && s.s.Enabled {
			out = append(out, s.name)
		}
	}
	return out
}

// Weight returns a step's relative weight, defaulting to 1 when none was written.
// Weight is a pointer so that an explicitly configured zero - which would mean a
// request that is never picked - is a validation error rather than a silent default.
func (s *HTTPStep) WeightOr() float64 { return weightOr(s.Weight) }

// WeightOr returns a journey's relative weight, defaulting to 1.
func (j *Journey) WeightOr() float64 { return weightOr(j.Weight) }

// WeightOr returns a query's relative weight, defaulting to 1.
func (q *Query) WeightOr() float64 { return weightOr(q.Weight) }

// WeightOr returns a command's relative weight, defaulting to 1.
func (c *Command) WeightOr() float64 { return weightOr(c.Weight) }

func weightOr(w *float64) float64 {
	if w == nil {
		return DefaultWeight
	}
	return *w
}
