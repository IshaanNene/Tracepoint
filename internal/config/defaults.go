package config

import "time"

// Defaults. Each is chosen to make the common case correct rather than merely legal.
const (
	DefaultDuration    = time.Minute
	DefaultBucket      = time.Second
	DefaultGrace       = 30 * time.Second
	DefaultTimeout     = 10 * time.Second
	DefaultMinSamples  = 20
	DefaultArrival     = "uniform"
	DefaultExecutor    = "arrival-rate"
	DefaultMaxInFlight = 256
	DefaultQueueDepth  = 64
	DefaultWeight      = 1.0
	DefaultMethod      = "GET"
	DefaultPick        = "per-iteration"
	DefaultFeederMode  = "sequential"
	DefaultRedisMode   = "single"
)

// ApplyDefaults fills in everything the document left unset. It runs only after the
// document has been accepted, so an error never describes a value the user did not
// write.
func (c *Config) ApplyDefaults() {
	c.applyCapacityDefaults()
	r := &c.Run
	if r.Duration == 0 {
		r.Duration = Duration(DefaultDuration)
	}
	if r.Bucket == 0 {
		r.Bucket = Duration(DefaultBucket)
	}
	if r.Grace == 0 {
		r.Grace = Duration(DefaultGrace)
	}
	if r.Timeout == 0 {
		r.Timeout = Duration(DefaultTimeout)
	}
	if r.MinSamples == 0 {
		r.MinSamples = DefaultMinSamples
	}
	if r.Arrival == "" {
		r.Arrival = DefaultArrival
	}

	if c.HTTP != nil {
		applyExecutorDefaults(&c.HTTP.Executor, r.Duration)
		for i := range c.HTTP.Requests {
			applyStepDefaults(&c.HTTP.Requests[i], true)
		}
		for i := range c.HTTP.Journeys {
			j := &c.HTTP.Journeys[i]
			for k := range j.Steps {
				// Only the journey carries a weight; its steps run in order.
				applyStepDefaults(&j.Steps[k], false)
			}
		}
		for i := range c.HTTP.Feeders {
			if c.HTTP.Feeders[i].Mode == "" {
				c.HTTP.Feeders[i].Mode = DefaultFeederMode
			}
		}
	}

	if c.DB != nil {
		applyExecutorDefaults(&c.DB.Executor, r.Duration)
		c.DB.Driver = NormaliseDriver(c.DB.Driver)

	}

	if c.Redis != nil {
		applyExecutorDefaults(&c.Redis.Executor, r.Duration)
		if c.Redis.Mode == "" {
			c.Redis.Mode = DefaultRedisMode
		}

	}
}

func applyStepDefaults(s *HTTPStep, _ bool) {
	if s.Method == "" {
		s.Method = DefaultMethod
	}
	for i := range s.Extract {
		if s.Extract[i].From == "" {
			s.Extract[i].From = "body"
		}
	}
}

func applyExecutorDefaults(e *Executor, runDuration Duration) {
	if e.Type == "" {
		e.Type = DefaultExecutor
	}
	if e.Type == DefaultExecutor {
		if e.MaxInFlight == 0 {
			e.MaxInFlight = DefaultMaxInFlight
		}
		if e.QueueDepth == nil {
			d := DefaultQueueDepth
			e.QueueDepth = &d
		}
		// The `rate:` shorthand means a constant rate held for the whole run, not a
		// ramp up to it. Expanding it here keeps that distinction in one place.
		if len(e.Stages) == 0 && e.Rate > 0 {
			e.Stages = []Stage{{Duration: runDuration, Target: e.Rate}}
		}
	}
	if e.Type == "vus" {
		if e.Pick == "" {
			e.Pick = DefaultPick
		}
		if len(e.Stages) == 0 && e.VUs > 0 {
			e.Stages = []Stage{{Duration: runDuration, Target: float64(e.VUs)}}
		}
	}
}

// StartRate is the rate a profile begins at.
//
// It is what separates the two configuration forms: `rate: 40` holds a flat 40 from
// the first instant, while an explicit `stages:` list ramps up from nothing to the
// first stage's target. Both compile to the same profile type, differing only here.
func (e *Executor) StartRate() float64 {
	if e.Rate > 0 && len(e.Stages) == 1 && e.Stages[0].Target == e.Rate {
		return e.Rate
	}
	if e.VUs > 0 && len(e.Stages) == 1 && e.Stages[0].Target == float64(e.VUs) {
		return float64(e.VUs)
	}
	return 0
}

// TotalDuration is how long the executor's stages run for.
func (e *Executor) TotalDuration() time.Duration {
	var total time.Duration
	for _, s := range e.Stages {
		total += s.Duration.D()
	}
	return total
}

// NormaliseDriver maps the names people actually write to the ones the drivers use.
func NormaliseDriver(name string) string {
	switch name {
	case "postgresql", "psql", "pg", "postgres":
		return "postgres"
	case "mysql", "mariadb":
		return "mysql"
	case "sqlite", "sqlite3":
		return "sqlite"
	default:
		return name
	}
}
