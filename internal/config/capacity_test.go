package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
)

const capacityDoc = `
version: 1
run: { bucket: 1s }
http:
  executor: { rate: 50 }
  requests:
    - { name: probe, url: "http://127.0.0.1:8080/" }
capacity: { knob: rate, max: 800 }
`

// Every default is filled in, and a search without run.duration may take as long as
// its plan could need.
func TestCapacityDefaults(t *testing.T) {
	cfg := mustLoad(t, capacityDoc, nil)
	cp := cfg.Capacity
	if cp.Runner != "http" || cp.Start != 50 || cp.StepDuration.D() != time.Minute ||
		cp.Settle.D() != 12*time.Second || cp.Cooldown.D() != 10*time.Second || cp.Refine != "bisect" || !*cp.Confirm {
		t.Fatalf("capacity = %+v", cp)
	}
	plan, err := cp.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if bound := cp.Bound(plan); cfg.Run.Duration.D() != bound || bound < 10*time.Minute {
		t.Fatalf("run.duration %s, bound %s", cfg.Run.Duration, bound)
	}
}

// An explicit run.duration is the search's budget, and an explicit stage list on the
// knob's runner no longer has to agree with it.
func TestCapacityBudget(t *testing.T) {
	cfg := mustLoad(t, strings.Replace(capacityDoc, "run: { bucket: 1s }", "run: { bucket: 1s, duration: 5m }", 1), nil)
	if cfg.Run.Duration.D() != 5*time.Minute {
		t.Fatalf("run.duration = %s", cfg.Run.Duration)
	}
	doc := `
version: 1
run: { duration: 5m }
http:
  executor: { stages: [{ duration: 10s, target: 20 }] }
  requests:
    - { name: probe, url: "http://127.0.0.1:8080/" }
capacity: { knob: rate, start: 20, max: 200 }
`
	mustLoad(t, doc, nil)
}

func TestCapacityRejects(t *testing.T) {
	cases := map[string]struct {
		capacity string
		path     string
	}{
		"no ceiling":          {`{ knob: rate }`, "/capacity/max"},
		"a missing runner":    {`{ knob: rate, runner: db, max: 100 }`, "/capacity/runner"},
		"the wrong knob":      {`{ knob: concurrency, max: 100 }`, "/capacity/knob"},
		"max below start":     {`{ knob: rate, start: 100, max: 50 }`, "/capacity/max"},
		"a bad refine":        {`{ knob: rate, max: 100, refine: "linear:0" }`, "/capacity/refine"},
		"settle past a step":  {`{ knob: rate, max: 100, step_duration: 10s, settle: 10s }`, "/capacity/settle"},
		"under one bucket":    {`{ knob: rate, max: 100, step_duration: 10s, settle: 9500ms }`, "/capacity/settle"},
		"a budget of nothing": {`{ knob: rate, max: 100, step_duration: 2m }`, "/run/duration"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(capacityDoc, "capacity: { knob: rate, max: 800 }", "capacity: "+c.capacity, 1)
			if name == "a budget of nothing" {
				doc = strings.Replace(doc, "run: { bucket: 1s }", "run: { bucket: 1s, duration: 1m }", 1)
			}
			_, err := load(t, doc, nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(errorPaths(err), c.path) {
				t.Fatalf("no problem at %s: %v", c.path, err)
			}
		})
	}
}

// The ceiling is what the search may offer, so the policy judges it.
func TestCapacityCeilingIsHeldToPolicy(t *testing.T) {
	cfg := mustLoad(t, capacityDoc, nil)
	granted := policy.Default()
	limit := 500.0
	granted.MaxRatePerRunner = &limit
	_, _, err := cfg.ApplyPolicy(granted)
	var e *errs.Error
	if !errors.As(err, &e) || !strings.Contains(errorPaths(err), "/capacity/max") {
		t.Fatalf("a ceiling of 800/s against a 500/s policy: %v", err)
	}
}

// errorPaths lists the paths of an error and every cause it carries.
func errorPaths(err error) string {
	var e *errs.Error
	if !errors.As(err, &e) {
		return ""
	}
	out := e.Path
	for _, c := range e.Causes {
		out += " " + c.Path
	}
	return out
}

// A defaulted budget longer than the policy allows is shortened, with a warning; one
// someone wrote is refused like any run.duration.
func TestCapacityBudgetMeetsThePolicy(t *testing.T) {
	doc := strings.Replace(capacityDoc, "max: 800", "max: 400", 1)
	cfg := mustLoad(t, doc, nil)
	_, warnings, err := cfg.ApplyPolicy(policy.ServerDefault())
	if err != nil {
		t.Fatalf("a defaulted budget was refused: %v", err)
	}
	if cfg.Run.Duration.D() != 10*time.Minute {
		t.Fatalf("run.duration = %s, want the policy's 10m", cfg.Run.Duration)
	}
	found := false
	for _, w := range warnings {
		found = found || w.Code == "CAPACITY_BUDGET_CLAMPED"
	}
	if !found {
		t.Fatalf("no warning: %+v", warnings)
	}

	cfg = mustLoad(t, strings.Replace(doc, "run: { bucket: 1s }", "run: { bucket: 1s, duration: 20m }", 1), nil)
	if _, _, err := cfg.ApplyPolicy(policy.ServerDefault()); err == nil {
		t.Fatal("an explicit 20m budget passed a 10m policy")
	}
}
