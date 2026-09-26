package config

import (
	"time"

	"github.com/IshaanNene/Tracepoint/internal/capacity"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// CodeCapacityBudgetClamped is the warning given when a policy's duration ceiling
// shortens a search's default budget.
const CodeCapacityBudgetClamped = "CAPACITY_BUDGET_CLAMPED"

// Capacity defaults (spec §5.6).
const (
	DefaultStepDuration = time.Minute
	DefaultSettleShare  = 0.2
	DefaultCooldown     = 10 * time.Second
	DefaultRefine       = "bisect"
)

// applyCapacityDefaults fills in a capacity search. It runs before the run duration is
// defaulted: a search whose run.duration was not given gets the longest the plan can
// take, so it is never cut short by a default nobody chose.
func (c *Config) applyCapacityDefaults() {
	cp := c.Capacity
	if cp == nil {
		return
	}
	if cp.Runner == "" {
		cp.Runner = "http"
	}
	if cp.StepDuration == nil {
		d := Duration(DefaultStepDuration)
		cp.StepDuration = &d
	}
	if cp.Settle == nil {
		d := Duration(time.Duration(float64(cp.StepDuration.D()) * DefaultSettleShare))
		cp.Settle = &d
	}
	if cp.Cooldown == nil {
		d := Duration(DefaultCooldown)
		cp.Cooldown = &d
	}
	if cp.Refine == "" {
		cp.Refine = DefaultRefine
	}
	if cp.Confirm == nil {
		t := true
		cp.Confirm = &t
	}
	if cp.Start == 0 {
		// The runner's own configured level is the natural place to begin.
		if ex := c.Executor(cp.Runner); ex != nil {
			switch {
			case ex.Rate > 0:
				cp.Start = ex.Rate
			case ex.VUs > 0:
				cp.Start = float64(ex.VUs)
			}
		}
	}
	if c.Run.Duration == 0 {
		if plan, err := cp.Plan(); err == nil {
			c.Run.Duration = Duration(cp.Bound(plan))
			cp.budgetDefaulted = true
		}
	}
}

// Plan is the search a capacity section describes.
func (cp *Capacity) Plan() (capacity.Plan, error) {
	linear, err := capacity.ParseRefine(cp.Refine)
	if err != nil {
		return capacity.Plan{}, err
	}
	p := capacity.Plan{Start: cp.Start, Max: cp.Max, Resolution: cp.Resolution, Linear: linear, Confirm: cp.Confirm == nil || *cp.Confirm}
	if _, err := capacity.NewSearch(p); err != nil {
		return capacity.Plan{}, err
	}
	return p, nil
}

// Bound is the longest a search can take: every level it could possibly run, each
// followed by a cooldown.
func (cp *Capacity) Bound(p capacity.Plan) time.Duration {
	per := cp.StepDuration.D()
	if cp.Cooldown != nil {
		per += cp.Cooldown.D()
	}
	return time.Duration(capacity.MaxLevels(p)) * per
}

// Steady is the measured part of each level.
func (cp *Capacity) Steady() time.Duration { return cp.StepDuration.D() - cp.Settle.D() }

func (c *Config) validateCapacity(add func(*errs.Error)) {
	cp := c.Capacity
	ex := c.Executor(cp.Runner)
	if ex == nil {
		add(invalid("/capacity/runner", "capacity.runner is %s, which is not configured", cp.Runner).
			WithHint("add a %s section, or point the search at a runner that exists", cp.Runner))
		return
	}
	switch {
	case cp.Knob == capacity.KnobRate && ex.Type != DefaultExecutor:
		add(invalid("/capacity/knob", "knob rate needs the arrival-rate executor, but %s uses %s", cp.Runner, ex.Type).
			WithHint("use knob: concurrency for a vus executor"))
	case cp.Knob == capacity.KnobConcurrency && ex.Type != "vus":
		add(invalid("/capacity/knob", "knob concurrency needs the vus executor, but %s uses %s", cp.Runner, ex.Type).
			WithHint("use knob: rate for an arrival-rate executor"))
	}
	if cp.Max == 0 {
		add(missing("/capacity/max", "capacity.max is required: the search needs a ceiling").
			WithHint("the highest %s that may be offered, such as max: 2000", cp.Knob))
		return
	}
	if cp.Start == 0 {
		add(missing("/capacity/start", "capacity.start is required when %s has no rate or vus to start from", cp.Runner))
		return
	}
	if _, err := cp.Plan(); err != nil {
		add(asCoded(err))
		return
	}
	step, settle := cp.StepDuration.D(), cp.Settle.D()
	switch {
	case step <= 0:
		add(invalid("/capacity/step_duration", "capacity.step_duration must be positive"))
	case settle < 0 || settle >= step:
		add(invalid("/capacity/settle", "capacity.settle (%s) must be shorter than step_duration (%s)", settle, step))
	case step-settle < c.Run.Bucket.D():
		add(invalid("/capacity/settle", "each level measures %s after settling, less than one %s bucket", step-settle, c.Run.Bucket.D()).
			WithHint("lengthen step_duration or shorten settle"))
	}
	if cp.Cooldown.D() < 0 {
		add(invalid("/capacity/cooldown", "capacity.cooldown must not be negative"))
	}
	if c.Run.Duration.D() < step {
		add(invalid("/run/duration", "run.duration (%s) is the search's time budget and cannot hold even one %s level", c.Run.Duration, step).
			WithHint("omit run.duration to allow the longest the search can take"))
	}
}
