package config

import (
	"fmt"
	"math"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Validate checks everything the schema cannot: that weights are usable, that stages
// add up, that names are unique, and that the configuration describes a run that could
// actually happen. Every problem found is reported, not just the first.
func (c *Config) Validate() error {
	var problems []*errs.Error
	add := func(e *errs.Error) { problems = append(problems, e) }

	c.validateRun(add)

	if len(c.Runners()) == 0 {
		add(errs.New(errs.CodeConfigNoRunner, "no runner is configured, so there is nothing to run").
			WithHint("add an http, db or redis section; `tracepoint init` writes a starter"))
	}
	if c.HTTP != nil {
		c.validateHTTP(add)
	}
	if c.DB != nil {
		c.validateDB(add)
	}
	if c.Redis != nil {
		c.validateRedis(add)
	}
	if c.Telemetry != nil {
		c.validateTelemetry(add)
	}
	c.validateDataflow(add)

	if len(problems) == 0 {
		return nil
	}
	return summarise(problems)
}

func (c *Config) validateRun(add func(*errs.Error)) {
	r := c.Run
	if r.Duration <= 0 {
		add(invalid("/run/duration", "the run duration must be positive, got %s", r.Duration).
			WithHint("for example `duration: 3m`"))
	}
	if r.Bucket <= 0 {
		add(invalid("/run/bucket", "the bucket width must be positive, got %s", r.Bucket))
	}
	if r.Warmup < 0 {
		add(invalid("/run/warmup", "warm-up must not be negative, got %s", r.Warmup))
	}
	if r.Duration > 0 && r.Bucket > 0 && r.Bucket > r.Duration {
		add(invalid("/run/bucket", "the bucket width %s is longer than the run itself (%s)", r.Bucket, r.Duration).
			WithHint("a run needs several buckets to show a timeline; try bucket: 1s"))
	}
	if r.Warmup >= r.Duration && r.Duration > 0 {
		add(invalid("/run/warmup", "warm-up (%s) covers the whole run (%s), so nothing would be measured", r.Warmup, r.Duration))
	}
	if r.MinSamples < 1 {
		add(invalid("/run/min_samples", "min_samples must be at least 1, got %d", r.MinSamples))
	}
	switch r.Arrival {
	case "", "uniform", "poisson":
	default:
		add(invalid("/run/arrival", "unknown arrival process %q", r.Arrival).
			WithHint("use uniform or poisson"))
	}
	c.validateSLO(add)
}

func (c *Config) validateSLO(add func(*errs.Error)) {
	for name, target := range map[string]SLOTarget{"http": c.SLO.HTTP, "db": c.SLO.DB, "redis": c.SLO.Redis} {
		base := "/slo/" + name
		if target.ErrorRate != nil && (*target.ErrorRate < 0 || *target.ErrorRate > 1) {
			add(invalid(base+"/error_rate", "an error rate budget must be between 0 and 1, got %v", *target.ErrorRate))
		}
		if target.P95 != nil && *target.P95 <= 0 {
			add(invalid(base+"/p95", "a latency budget must be positive, got %s", *target.P95))
		}
		if target.P99 != nil && *target.P99 <= 0 {
			add(invalid(base+"/p99", "a latency budget must be positive, got %s", *target.P99))
		}
		if target.P95 != nil && target.P99 != nil && *target.P95 > *target.P99 {
			add(invalid(base+"/p95", "the p95 budget (%s) is looser than the p99 budget (%s)", *target.P95, *target.P99).
				WithHint("p95 should be at or below p99, since 95%% of requests are a subset of 99%%"))
		}
	}
}

func (c *Config) validateHTTP(add func(*errs.Error)) {
	h := c.HTTP
	switch {
	case len(h.Requests) > 0 && len(h.Journeys) > 0:
		add(errs.New(errs.CodeConfigRequestsAndJourney,
			"the http section has both `requests` and `journeys`").
			WithPath("/http").
			WithHint("pick one: `requests` for independent calls, `journeys` for ordered multi-step flows"))
	case len(h.Requests) == 0 && len(h.Journeys) == 0:
		add(errs.New(errs.CodeConfigMissingField, "the http section has no requests and no journeys").
			WithPath("/http").
			WithHint("add at least one request under `requests:`"))
	}

	seen := map[string]bool{}
	for i := range h.Requests {
		c.validateStep(&h.Requests[i], fmt.Sprintf("/http/requests/%d", i), true, seen, add)
	}
	for i := range h.Journeys {
		j := &h.Journeys[i]
		path := fmt.Sprintf("/http/journeys/%d", i)
		if j.Name == "" {
			add(missing(path+"/name", "a journey needs a name"))
		}
		if seen[j.Name] {
			add(duplicate(path+"/name", j.Name))
		}
		seen[j.Name] = true
		if err := checkWeight(j.Weight, path+"/weight"); err != nil {
			add(err)
		}
		if len(j.Steps) == 0 {
			add(missing(path+"/steps", "journey %q has no steps", j.Name))
		}
		stepNames := map[string]bool{}
		for k := range j.Steps {
			c.validateStep(&j.Steps[k], fmt.Sprintf("%s/steps/%d", path, k), false, stepNames, add)
		}
	}

	feeders := map[string]bool{}
	for i, f := range h.Feeders {
		path := fmt.Sprintf("/http/feeders/%d", i)
		if f.Name == "" {
			add(missing(path+"/name", "a feeder needs a name"))
		}
		if f.File == "" {
			add(missing(path+"/file", "feeder %q needs a file", f.Name))
		}
		if feeders[f.Name] {
			add(duplicate(path+"/name", f.Name))
		}
		feeders[f.Name] = true
		switch f.Mode {
		case "", "sequential", "random", "unique":
		default:
			add(invalid(path+"/mode", "unknown feeder mode %q", f.Mode).
				WithHint("use sequential, random or unique"))
		}
	}

	c.validateExecutor(&h.Executor, "/http/executor", add)
}

func (c *Config) validateStep(s *HTTPStep, path string, weighted bool, seen map[string]bool, add func(*errs.Error)) {
	if s.Name == "" {
		add(missing(path+"/name", "every request and step needs a name; it is how its statistics are grouped"))
	} else if seen[s.Name] {
		add(duplicate(path+"/name", s.Name))
	}
	seen[s.Name] = true

	if weighted {
		if err := checkWeight(s.Weight, path+"/weight"); err != nil {
			add(err)
		}
	} else if s.Weight != nil {
		add(invalid(path+"/weight", "journey steps run in order, so a step cannot carry a weight").
			WithHint("put the weight on the journey instead"))
	}
	if s.URL == "" {
		add(missing(path+"/url", "request %q needs a url", s.Name))
	}
	if s.Body != "" && s.BodyFile != "" {
		add(invalid(path+"/body", "request %q sets both body and body_file", s.Name).
			WithHint("pick one"))
	}
	if s.Timeout != nil && *s.Timeout <= 0 {
		add(invalid(path+"/timeout", "a timeout must be positive, got %s", *s.Timeout))
	}
	if s.Expect != nil {
		for k, code := range s.Expect.Status {
			if code < 100 || code > 599 {
				add(invalid(fmt.Sprintf("%s/expect/status/%d", path, k),
					"%d is not an HTTP status code", code))
			}
		}
	}
	for k, e := range s.Extract {
		p := fmt.Sprintf("%s/extract/%d", path, k)
		if e.Name == "" {
			add(missing(p+"/name", "an extraction needs a name"))
		}
		switch e.From {
		case "", "body", "header", "status":
		default:
			add(invalid(p+"/from", "unknown extraction source %q", e.From).
				WithHint("use body, header or status"))
		}
		if e.Path == "" && e.From != "status" {
			add(missing(p+"/path", "extraction %q needs a path", e.Name))
		}
	}
	if s.Think != nil {
		c.validateThink(s.Think, path+"/think", add)
	}
}

func (c *Config) validateThink(t *ThinkTime, path string, add func(*errs.Error)) {
	switch t.Type {
	case "constant":
		if t.Duration == nil {
			add(missing(path+"/duration", "constant think time needs a duration"))
		}
	case "uniform":
		if t.Min == nil || t.Max == nil {
			add(missing(path+"/min", "uniform think time needs min and max"))
		} else if *t.Min > *t.Max {
			add(invalid(path+"/min", "min think time (%s) is above max (%s)", *t.Min, *t.Max))
		}
	case "exponential":
		if t.Mean == nil {
			add(missing(path+"/mean", "exponential think time needs a mean"))
		}
	default:
		add(invalid(path+"/type", "unknown think time %q", t.Type).
			WithHint("use constant, uniform or exponential"))
	}
}

func (c *Config) validateExecutor(e *Executor, path string, add func(*errs.Error)) {
	switch e.Type {
	case "", "arrival-rate":
		if e.VUs > 0 {
			add(invalid(path+"/vus", "an arrival-rate executor cannot take a vu count").
				WithHint("use `rate:` for an open model, or set `type: vus` for a closed one"))
		}
		if e.MaxInFlight < 0 {
			add(invalid(path+"/max_in_flight", "max_in_flight must be positive, got %d", e.MaxInFlight))
		}
	case "vus":
		if e.Rate > 0 {
			add(invalid(path+"/rate", "a vus executor cannot take a rate").
				WithHint("use `vus:` for a closed model, or set `type: arrival-rate` for an open one"))
		}
		switch e.Pick {
		case "", "per-iteration", "per-vu":
		default:
			add(invalid(path+"/pick", "unknown pick mode %q", e.Pick).
				WithHint("use per-iteration or per-vu"))
		}
	default:
		add(invalid(path+"/type", "unknown executor type %q", e.Type).
			WithHint("use arrival-rate or vus"))
	}

	if e.Rate > 0 && len(e.Stages) > 1 {
		add(invalid(path+"/rate", "`rate` is shorthand for a single constant stage and cannot be combined with `stages`").
			WithHint("remove `rate:` and express the constant part as a stage"))
	}
	if len(e.Stages) == 0 && e.Rate == 0 && e.VUs == 0 {
		add(missing(path, "the executor has no rate, no vu count and no stages").
			WithHint("add `rate: 50` for a constant open-model load"))
	}
	for i, s := range e.Stages {
		p := fmt.Sprintf("%s/stages/%d", path, i)
		if s.Duration <= 0 {
			add(invalid(p+"/duration", "a stage needs a positive duration, got %s", s.Duration))
		}
		if s.Target < 0 || math.IsNaN(s.Target) || math.IsInf(s.Target, 0) {
			add(invalid(p+"/target", "a stage target must be a finite non-negative number, got %v", s.Target))
		}
	}
	// Stages define the shape of the load; the run duration says how long to measure.
	// If they disagree the run would either stop mid-ramp or idle at the end, and
	// either way the result would not be the test that was written.
	if total := e.TotalDuration(); len(e.Stages) > 0 && c.Run.Duration > 0 && total != c.Run.Duration.D() {
		add(errs.New(errs.CodeConfigStagesMismatch,
			"the executor's stages last %s but the run is %s", total, c.Run.Duration).
			WithPath(path + "/stages").
			WithHint("stage durations must sum to run.duration"))
	}
}

func (c *Config) validateDB(add func(*errs.Error)) {
	d := c.DB
	switch NormaliseDriver(d.Driver) {
	case "postgres", "mysql", "sqlite":
	case "":
		add(missing("/db/driver", "the db section needs a driver"))
	default:
		add(invalid("/db/driver", "unknown driver %q", d.Driver).
			WithHint("this build supports postgres, mysql and sqlite"))
	}
	if d.DSN == "" {
		add(missing("/db/dsn", "the db section needs a dsn").
			WithHint("use ${PG_DSN} so the password stays out of the file"))
	}
	if len(d.Queries) == 0 {
		add(missing("/db/queries", "the db section has no queries"))
	}
	seen := map[string]bool{}
	for i, q := range d.Queries {
		path := fmt.Sprintf("/db/queries/%d", i)
		if q.Name == "" {
			add(missing(path+"/name", "a query needs a name"))
		} else if seen[q.Name] {
			add(duplicate(path+"/name", q.Name))
		}
		seen[q.Name] = true
		switch {
		case q.SQL == "" && len(q.Tx) == 0:
			add(missing(path+"/sql", "query %q has neither sql nor tx", q.Name))
		case q.SQL != "" && len(q.Tx) > 0:
			add(invalid(path+"/sql", "query %q sets both sql and tx", q.Name).WithHint("pick one"))
		}
		if q.Type != "" && q.Type != "read" && q.Type != "write" {
			add(invalid(path+"/type", "unknown query type %q", q.Type).WithHint("use read or write"))
		}
		if err := checkWeight(q.Weight, path+"/weight"); err != nil {
			add(err)
		}
	}
	if d.Pool != nil && d.Pool.MaxOpen < 0 {
		add(invalid("/db/pool/max_open", "max_open must be positive, got %d", d.Pool.MaxOpen))
	}
	c.validateExecutor(&d.Executor, "/db/executor", add)
}

func (c *Config) validateRedis(add func(*errs.Error)) {
	r := c.Redis
	switch r.Mode {
	case "", "single":
		if r.Addr == "" && len(r.Addrs) == 0 {
			add(missing("/redis/addr", "the redis section needs an addr"))
		}
	case "cluster":
		if len(r.Addrs) == 0 {
			add(missing("/redis/addrs", "cluster mode needs addrs"))
		}
	case "sentinel":
		if len(r.Addrs) == 0 {
			add(missing("/redis/addrs", "sentinel mode needs addrs"))
		}
		if r.MasterName == "" {
			add(missing("/redis/master_name", "sentinel mode needs a master_name"))
		}
	default:
		add(invalid("/redis/mode", "unknown redis mode %q", r.Mode).
			WithHint("use single, cluster or sentinel"))
	}
	if r.Addr != "" && len(r.Addrs) > 0 {
		add(invalid("/redis/addr", "the redis section sets both addr and addrs").WithHint("pick one"))
	}
	if len(r.Commands) == 0 {
		add(missing("/redis/commands", "the redis section has no commands"))
	}
	seen := map[string]bool{}
	for i, cmd := range r.Commands {
		path := fmt.Sprintf("/redis/commands/%d", i)
		if cmd.Name == "" {
			add(missing(path+"/name", "a command needs a name"))
		} else if seen[cmd.Name] {
			add(duplicate(path+"/name", cmd.Name))
		}
		seen[cmd.Name] = true
		switch {
		case len(cmd.Cmd) == 0 && len(cmd.Pipeline) == 0:
			add(missing(path+"/cmd", "command %q has neither cmd nor pipeline", cmd.Name))
		case len(cmd.Cmd) > 0 && len(cmd.Pipeline) > 0:
			add(invalid(path+"/cmd", "command %q sets both cmd and pipeline", cmd.Name).WithHint("pick one"))
		}
		if err := checkWeight(cmd.Weight, path+"/weight"); err != nil {
			add(err)
		}
	}
	c.validateExecutor(&r.Executor, "/redis/executor", add)
}

// checkWeight rejects a weight that was written but cannot be used. Absent is fine and
// means 1; zero or negative means the item can never be picked, which is a mistake
// worth reporting rather than a way to disable something.
// validateTelemetry checks that every enabled sampler has something to connect to. A
// sampler borrows its runner's connection settings unless it is given its own dsn, so
// a postgres sampler beside a mysql runner, say, has nowhere to go.
func (c *Config) validateTelemetry(add func(*errs.Error)) {
	t := c.Telemetry
	if t.Interval != nil && *t.Interval <= 0 {
		add(invalid("/telemetry/interval", "the sampling interval must be positive, got %s", *t.Interval))
	}
	check := func(name string, s *Sampler, has bool, hint string) {
		if s == nil || !s.Enabled {
			return
		}
		if s.Interval != nil && *s.Interval <= 0 {
			add(invalid("/telemetry/"+name+"/interval", "the sampling interval must be positive, got %s", *s.Interval))
		}
		if s.DSN == "" && !has {
			add(missing("/telemetry/"+name+"/dsn", "the %s sampler has nothing to connect to", name).WithHint("%s", hint))
		}
	}
	driver := ""
	if c.DB != nil {
		driver = NormaliseDriver(c.DB.Driver)
	}
	check("postgres", t.Postgres, driver == "postgres",
		"set telemetry.postgres.dsn, or use it alongside a db section whose driver is postgres")
	check("mysql", t.MySQL, driver == "mysql",
		"set telemetry.mysql.dsn, or use it alongside a db section whose driver is mysql")
	check("redis", t.Redis, c.Redis != nil,
		"set telemetry.redis.dsn, or use it alongside a redis section")
}

func checkWeight(w *float64, path string) *errs.Error {
	if w == nil {
		return nil
	}
	if *w <= 0 || math.IsNaN(*w) || math.IsInf(*w, 0) {
		return invalid(path, "a weight must be above 0, got %v", *w).
			WithHint("remove the item instead of giving it a weight of zero")
	}
	return nil
}

func invalid(path, format string, args ...any) *errs.Error {
	return errs.New(errs.CodeConfigInvalidValue, format, args...).WithPath(path)
}

func missing(path, format string, args ...any) *errs.Error {
	return errs.New(errs.CodeConfigMissingField, format, args...).WithPath(path)
}

func duplicate(path, name string) *errs.Error {
	return errs.New(errs.CodeConfigDuplicateName, "duplicate name %q", name).
		WithPath(path).
		WithHint("names group statistics, so two of the same would merge silently")
}

// Labels lists a runner's label names in configuration order, which is what the
// collector resolves to identifiers before the run starts.
func (c *Config) Labels(runner string) []string {
	var out []string
	switch runner {
	case "http":
		if c.HTTP == nil {
			return nil
		}
		for _, r := range c.HTTP.Requests {
			out = append(out, r.Name)
		}
		for _, j := range c.HTTP.Journeys {
			for _, s := range j.Steps {
				out = append(out, j.Name+"/"+s.Name)
			}
		}
	case "db":
		if c.DB == nil {
			return nil
		}
		for _, q := range c.DB.Queries {
			out = append(out, q.Name)
		}
	case "redis":
		if c.Redis == nil {
			return nil
		}
		for _, cmd := range c.Redis.Commands {
			out = append(out, cmd.Name)
		}
	}
	return out
}

// Executor returns a runner's executor configuration.
func (c *Config) Executor(runner string) *Executor {
	switch runner {
	case "http":
		if c.HTTP != nil {
			return &c.HTTP.Executor
		}
	case "db":
		if c.DB != nil {
			return &c.DB.Executor
		}
	case "redis":
		if c.Redis != nil {
			return &c.Redis.Executor
		}
	}
	return nil
}

// Kind says whether a runner drives the application tier or probes one behind it.
func Kind(runner string) string {
	if runner == "http" {
		return "app"
	}
	return "storage"
}
