package plan

import (
	"fmt"
	"strings"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/policy"
)

// Plan is what a run would do, without doing any of it.
//
// It exists so that a person - or an agent about to spend ten minutes of wall clock -
// can see the offered load, the targets, the effective safety envelope and anything
// that was flagged, before committing. Everything in it is derived from the
// configuration and the policy; nothing is contacted.
type Plan struct {
	RunID        string    `json:"run_id,omitempty"`
	ConfigPath   string    `json:"config_path,omitempty"`
	Overrides    []string  `json:"overrides,omitempty"`
	DurationS    float64   `json:"duration_s"`
	BucketS      float64   `json:"bucket_s"`
	WarmupS      float64   `json:"warmup_s,omitempty"`
	Arrival      string    `json:"arrival"`
	Seed         *uint64   `json:"seed,omitempty"`
	Runners      []Runner  `json:"runners"`
	Targets      []string  `json:"targets"`
	SLO          []SLO     `json:"slo,omitempty"`
	Safety       Safety    `json:"safety"`
	Warnings     []Warning `json:"warnings,omitempty"`
	TotalOffered float64   `json:"total_offered_operations"`
}

// Runner is one tier's share of the plan.
type Runner struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Driver   string  `json:"driver,omitempty"`
	Executor string  `json:"executor"`
	Profile  string  `json:"profile"`
	PeakRPS  float64 `json:"peak_rps"`
	// PeakVUs is the most users a closed-model (vus) runner reaches; its offered
	// rate is an outcome, so PeakRPS and Offered are zero.
	PeakVUs float64  `json:"peak_vus,omitempty"`
	Offered float64  `json:"offered_operations"`
	Labels  []string `json:"labels"`
	Writes  []string `json:"writes,omitempty"`
}

// SLO is one configured budget.
type SLO struct {
	Runner string `json:"runner"`
	Metric string `json:"metric"`
	Budget string `json:"budget"`
}

// Safety is the envelope the run would execute under.
type Safety struct {
	AllowWrites      bool     `json:"allow_writes"`
	AllowDangerous   bool     `json:"allow_dangerous"`
	AllowInsecureTLS bool     `json:"allow_insecure_tls"`
	AllowTargets     []string `json:"allow_targets,omitempty"`
	MaxRatePerRunner *float64 `json:"max_rate_per_runner,omitempty"`
	MaxDuration      *string  `json:"max_duration,omitempty"`
}

// Warning is something worth knowing before starting.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Build describes what a configuration would do under a policy.
func Build(cfg *config.Config, effective policy.Policy, warnings []config.Warning, path string, overrides []string) Plan {
	p := Plan{
		ConfigPath: path,
		Overrides:  overrides,
		DurationS:  cfg.Run.Duration.D().Seconds(),
		BucketS:    cfg.Run.Bucket.D().Seconds(),
		WarmupS:    cfg.Run.Warmup.D().Seconds(),
		Arrival:    cfg.Run.Arrival,
		Seed:       cfg.Run.Seed,
		Targets:    cfg.Targets(),
		Safety: Safety{
			AllowWrites:      effective.AllowWrites,
			AllowDangerous:   effective.AllowDangerous,
			AllowInsecureTLS: effective.AllowInsecureTLS,
			AllowTargets:     effective.AllowTargets,
			MaxRatePerRunner: effective.MaxRatePerRunner,
			MaxDuration:      effective.MaxDuration,
		},
	}

	writes, _, _ := cfg.ClassifyForPlan()
	for _, name := range cfg.Runners() {
		ex := cfg.Executor(name)
		if ex == nil {
			continue
		}
		pr := Runner{
			Name: name, Kind: config.Kind(name),
			Executor: ex.Type, Profile: describeProfile(ex),
			PeakRPS: config.PeakRate(ex), Labels: cfg.Labels(name),
			Writes: writes[name],
		}
		pr.Offered = offeredOperations(ex)
		if ex.Type == "vus" {
			pr.PeakVUs, pr.PeakRPS = pr.PeakRPS, 0
		}
		p.TotalOffered += pr.Offered
		if name == "db" && cfg.DB != nil {
			pr.Driver = config.NormaliseDriver(cfg.DB.Driver)
		}
		p.Runners = append(p.Runners, pr)
	}

	for _, name := range []string{"http", "db", "redis"} {
		t := cfg.SLO.For(name)
		if t.P95 != nil {
			p.SLO = append(p.SLO, SLO{Runner: name, Metric: "p95", Budget: t.P95.String()})
		}
		if t.P99 != nil {
			p.SLO = append(p.SLO, SLO{Runner: name, Metric: "p99", Budget: t.P99.String()})
		}
		if t.ErrorRate != nil {
			p.SLO = append(p.SLO, SLO{Runner: name, Metric: "error_rate", Budget: fmt.Sprintf("%.2f%%", *t.ErrorRate*100)})
		}
	}

	for _, w := range warnings {
		p.Warnings = append(p.Warnings, Warning{Code: w.Code, Message: w.Message, Fix: w.Fix})
	}
	return p
}

// offeredOperations integrates the rate profile, which is the number of operations the
// run will actually offer - the figure worth knowing before starting a long test
// against someone else's system.
func offeredOperations(e *config.Executor) float64 {
	if e.Type == "vus" {
		return 0 // a closed model's output is a result, not an input
	}
	var total float64
	prev := e.StartRate()
	for _, s := range e.Stages {
		total += (prev + s.Target) / 2 * s.Duration.D().Seconds()
		prev = s.Target
	}
	return total
}

func describeProfile(e *config.Executor) string {
	if len(e.Stages) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(e.Stages))
	prev := e.StartRate()
	for _, s := range e.Stages {
		if s.Target == prev {
			parts = append(parts, fmt.Sprintf("hold %g for %s", s.Target, s.Duration))
		} else {
			parts = append(parts, fmt.Sprintf("%g to %g over %s", prev, s.Target, s.Duration))
		}
		prev = s.Target
	}
	return strings.Join(parts, ", then ")
}

// Render writes the plan for a person.
func Render(p Plan) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "\nPlan for %s\n", p.ConfigPath)
	fmt.Fprintf(b, "  %s over %s, %s buckets", p.Arrival, dur(p.DurationS), dur(p.BucketS))
	if p.WarmupS > 0 {
		fmt.Fprintf(b, ", %s warm-up", dur(p.WarmupS))
	}
	b.WriteString("\n\n")

	for _, r := range p.Runners {
		fmt.Fprintf(b, "  %-6s %-8s %s\n", r.Name, r.Kind, r.Profile)
		if r.Executor == "vus" {
			fmt.Fprintf(b, "         up to %.0f users, each looping as fast as the target answers; %d label(s)\n", r.PeakVUs, len(r.Labels))
		} else {
			fmt.Fprintf(b, "         peak %.0f/s, about %.0f operations, %d label(s)\n", r.PeakRPS, r.Offered, len(r.Labels))
		}
		if len(r.Writes) > 0 {
			fmt.Fprintf(b, "         writes: %s\n", strings.Join(r.Writes, ", "))
		}
	}
	if len(p.Targets) > 0 {
		fmt.Fprintf(b, "\n  targets: %s\n", strings.Join(p.Targets, ", "))
	}
	for _, s := range p.SLO {
		fmt.Fprintf(b, "  slo: %s %s <= %s\n", s.Runner, s.Metric, s.Budget)
	}

	fmt.Fprintf(b, "\n  safety: writes=%v dangerous=%v insecure_tls=%v\n",
		p.Safety.AllowWrites, p.Safety.AllowDangerous, p.Safety.AllowInsecureTLS)
	if p.Safety.MaxRatePerRunner != nil {
		fmt.Fprintf(b, "          max %g operations per second per runner\n", *p.Safety.MaxRatePerRunner)
	}

	for _, w := range p.Warnings {
		fmt.Fprintf(b, "\n  ! %s\n", w.Message)
		if w.Fix != "" {
			fmt.Fprintf(b, "    %s\n", w.Fix)
		}
	}
	fmt.Fprintf(b, "\n  nothing was contacted and no load was generated.\n\n")
	return b.String()
}

func dur(seconds float64) string {
	return time.Duration(seconds * float64(time.Second)).String()
}
