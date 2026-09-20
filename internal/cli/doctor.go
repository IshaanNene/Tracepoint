package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
)

// Diagnosis is everything doctor found.
type Diagnosis struct {
	ConfigPath string     `json:"config_path"`
	OK         bool       `json:"ok"`
	Findings   []Finding  `json:"findings"`
	Host       HostReport `json:"host"`
	Targets    []Target   `json:"targets"`
}

// Finding is one thing doctor noticed. Every finding has a code, a severity and a fix,
// because a diagnosis that does not say what to do is just a complaint.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

// HostReport describes the machine the load would be generated from. A generator that
// cannot keep up produces a run that measures itself, so its limits are worth knowing
// before the run rather than after.
type HostReport struct {
	CPUs          int `json:"cpus"`
	GOMAXPROCS    int `json:"gomaxprocs"`
	OpenFileLimit int `json:"open_file_limit,omitempty"`
	PlannedConns  int `json:"planned_connections"`
}

// Target is a resolved destination and what the policy makes of it.
type Target struct {
	Host    string   `json:"host"`
	Addrs   []string `json:"addrs,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Allowed bool     `json:"allowed"`
	Reason  string   `json:"reason,omitempty"`
}

func newDoctorCmd(env Env, g *globals) *cobra.Command {
	var (
		configPath string
		policyPath string
		overrides  []string
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that a run would work, before spending the time on it",
		Long: strings.TrimSpace(`
Resolve and classify every target, connect to each one, and check that this machine can
generate the load the configuration asks for. Nothing is loaded and no data is changed:
statements are prepared rather than executed, and every check is a single round trip.

Every finding carries a code, a severity and the change that would fix it. A run is
usually worth a doctor first - the checks take a second, and the failures they catch
would otherwise be discovered several minutes into a test.`),
		Example: strings.TrimSpace(`
  tracepoint doctor -c tracepoint.yaml
  tracepoint doctor -c tracepoint.yaml --output json`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, effective, warnings, err := prepare(cmd.Context(), env, configPath, policyPath, overrides)
			if err != nil {
				return err
			}
			d := diagnose(cmd.Context(), cfg, effective, warnings, configPath)

			if g.json() {
				if err := writeJSON(env.Stdout, d); err != nil {
					return err
				}
			} else if _, err := io.WriteString(env.Stdout, renderDiagnosis(d)); err != nil {
				return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the diagnosis")
			}
			if !d.OK {
				return &exitError{code: errs.ExitRuntime, documented: true,
					err: errs.New(errs.CodePreflightConnect, "the configuration would not run as written")}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&configPath, "config", "c", "tracepoint.yaml", "configuration file, or - to read standard input")
	f.StringVar(&policyPath, "policy", "", "safety policy file to check against")
	f.StringArrayVar(&overrides, "set", nil, "override a configuration value, as path=value")
	return cmd
}

// diagnose runs every check. It never generates load and never changes data.
func diagnose(ctx context.Context, cfg *config.Config, effective policy.Policy, warnings []config.Warning, path string) Diagnosis {
	d := Diagnosis{ConfigPath: path, OK: true}

	for _, w := range warnings {
		d.add(Finding{Code: w.Code, Severity: "warn", Message: w.Message, Fix: w.Fix})
	}

	d.Host = hostReport(cfg)
	d.checkHost()
	d.checkTargets(ctx, cfg, effective)
	d.checkConfiguration(cfg)

	sort.SliceStable(d.Findings, func(i, j int) bool {
		return severityRank(d.Findings[i].Severity) > severityRank(d.Findings[j].Severity)
	})
	return d
}

func severityRank(s string) int {
	switch s {
	case "error":
		return 2
	case "warn":
		return 1
	default:
		return 0
	}
}

func (d *Diagnosis) add(f Finding) {
	if f.Severity == "error" {
		d.OK = false
	}
	d.Findings = append(d.Findings, f)
}

func hostReport(cfg *config.Config) HostReport {
	h := HostReport{CPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0)}
	if n, ok := openFileLimit(); ok {
		h.OpenFileLimit = n
	}
	// Every in-flight operation may hold a connection, so the concurrency ceilings
	// across all runners are what the descriptor limit has to cover.
	for _, name := range cfg.Runners() {
		if ex := cfg.Executor(name); ex != nil && ex.MaxInFlight > 0 {
			h.PlannedConns += ex.MaxInFlight
		}
	}
	return h
}

// checkHost looks for limits on this machine that would make the generator, rather
// than the target, the thing being measured.
func (d *Diagnosis) checkHost() {
	h := d.Host
	if h.OpenFileLimit > 0 && h.PlannedConns > 0 {
		// Each connection is a descriptor, and so is every file the process already has
		// open; leaving headroom avoids failing at the very end of a ramp.
		const headroom = 128
		if h.PlannedConns+headroom > h.OpenFileLimit {
			d.add(Finding{
				Code: "OPEN_FILE_LIMIT", Severity: "error",
				Message: fmt.Sprintf(
					"the configuration may hold %d connections but this machine allows %d open files",
					h.PlannedConns, h.OpenFileLimit),
				Fix: fmt.Sprintf("raise the limit with `ulimit -n %d`, or lower max_in_flight", h.PlannedConns+headroom),
			})
		}
	}
	if h.PlannedConns > 0 && h.CPUs > 0 && h.PlannedConns > h.CPUs*512 {
		d.add(Finding{
			Code: "CONCURRENCY_ABOVE_HOST", Severity: "warn",
			Message: fmt.Sprintf("%d operations in flight on %d CPUs is a lot of goroutine scheduling",
				h.PlannedConns, h.CPUs),
			Fix: "if dispatch lag makes the run invalid, lower max_in_flight or generate load from a bigger machine",
		})
	}
}

// checkTargets resolves each host, judges it against the policy and opens a connection.
func (d *Diagnosis) checkTargets(ctx context.Context, cfg *config.Config, effective policy.Policy) {
	for _, host := range cfg.Targets() {
		t := Target{Host: host}
		resolved, err := policy.Resolve(ctx, nil, host)
		if err != nil {
			t.Reason = err.Error()
			d.Targets = append(d.Targets, t)
			d.add(Finding{
				Code: "PREFLIGHT_DNS", Severity: "error",
				Message: fmt.Sprintf("%s does not resolve", host),
				Fix:     "check the hostname and this machine's name resolution",
			})
			continue
		}
		t.Addrs, t.Scope = resolved.Addrs, string(resolved.Scope)

		if err := effective.CheckTarget(resolved); err != nil {
			t.Reason = err.Error()
			d.Targets = append(d.Targets, t)
			var typed *errs.Error
			code := "POLICY_TARGET_NOT_ALLOWED"
			fix := ""
			if errors.As(err, &typed) {
				code, fix = string(typed.Code), typed.Hint
			}
			d.add(Finding{Code: code, Severity: "error",
				Message: fmt.Sprintf("%s is not allowed by the policy", host), Fix: fix})
			continue
		}
		t.Allowed = true
		d.Targets = append(d.Targets, t)

		// Generator and target sharing a machine compete for the same CPU, so the
		// numbers are indicative rather than a capacity measurement.
		if resolved.Scope == policy.ScopeLoopback {
			d.add(Finding{
				Code: "SAME_HOST_TARGET", Severity: "warn",
				Message: fmt.Sprintf("%s is on this machine, so the generator and the target will share its CPU", host),
				Fix:     "results will be indicative rather than a capacity number; run the generator elsewhere to measure capacity",
			})
		}
	}
}

// checkConfiguration looks for settings that would make a run hard to interpret.
func (d *Diagnosis) checkConfiguration(cfg *config.Config) {
	for _, name := range cfg.Runners() {
		ex := cfg.Executor(name)
		if ex == nil || ex.Type == "vus" {
			continue
		}
		// Little's Law: concurrency must cover rate multiplied by service time. A
		// ceiling below the offered rate cannot sustain even one second of latency, and
		// the run would be capped by us rather than by the target.
		peak := config.PeakRate(ex)
		if ex.MaxInFlight > 0 && peak > 0 && float64(ex.MaxInFlight) < peak*0.1 {
			d.add(Finding{
				Code: "LITTLES_LAW_INCONSISTENT", Severity: "warn",
				Message: fmt.Sprintf(
					"%s offers %.0f operations per second with only %d in flight, which cannot be sustained unless each finishes within %.0fms",
					name, peak, ex.MaxInFlight, float64(ex.MaxInFlight)/peak*1000),
				Fix: fmt.Sprintf("raise %s.executor.max_in_flight, or expect the run to be marked invalid as client-capped", name),
			})
		}
	}

	if cfg.DB != nil && cfg.DB.Pool != nil {
		workers := cfg.DB.Executor.MaxInFlight
		if workers > 0 && cfg.DB.Pool.MaxOpen > 0 && cfg.DB.Pool.MaxOpen < workers {
			d.add(Finding{
				Code: "POOL_UNDERSIZED", Severity: "warn",
				Message: fmt.Sprintf("the db pool allows %d connections for %d workers, so operations will queue inside the client",
					cfg.DB.Pool.MaxOpen, workers),
				Fix: "raise db.pool.max_open to at least the worker count, or the wait will look like database latency",
			})
		}
	}
	if cfg.Redis != nil && cfg.Redis.Pool != nil {
		workers := cfg.Redis.Executor.MaxInFlight
		if workers > 0 && cfg.Redis.Pool.Size > 0 && cfg.Redis.Pool.Size < workers {
			d.add(Finding{
				Code: "POOL_UNDERSIZED", Severity: "warn",
				Message: fmt.Sprintf("the redis pool allows %d connections for %d workers", cfg.Redis.Pool.Size, workers),
				Fix:     "raise redis.pool.size to at least the worker count",
			})
		}
	}

	if cfg.SLO.IsZero() {
		d.add(Finding{
			Code: "NO_SLO", Severity: "info",
			Message: "no budgets are configured, so the run cannot pass or fail",
			Fix:     "add an slo section to make the run a gate rather than a measurement",
		})
	}
	if cfg.Run.Warmup == 0 {
		d.add(Finding{
			Code: "NO_WARMUP", Severity: "info",
			Message: "no warm-up is configured, so pool filling and cold caches count towards the result",
			Fix:     "set run.warmup to a few seconds",
		})
	}
	// Storage runners are what make tier attribution possible at all.
	if cfg.HTTP != nil && cfg.DB == nil && cfg.Redis == nil {
		d.add(Finding{
			Code: "NO_STORAGE_PROBE", Severity: "info",
			Message: "only the application tier is driven, so a slowdown cannot be attributed to a tier",
			Fix:     "add a db or redis section; those runners probe the tier while they load it, which is what makes attribution possible",
		})
	}
}

func renderDiagnosis(d Diagnosis) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "\nDoctor: %s\n\n", d.ConfigPath)

	fmt.Fprintf(b, "  host    %d CPUs, GOMAXPROCS %d", d.Host.CPUs, d.Host.GOMAXPROCS)
	if d.Host.OpenFileLimit > 0 {
		fmt.Fprintf(b, ", %d open files allowed", d.Host.OpenFileLimit)
	}
	if d.Host.PlannedConns > 0 {
		fmt.Fprintf(b, ", up to %d connections planned", d.Host.PlannedConns)
	}
	b.WriteString("\n")

	for _, t := range d.Targets {
		status := "ok"
		if !t.Allowed {
			status = "refused"
		}
		fmt.Fprintf(b, "  target  %-28s %-12s %s\n", t.Host, t.Scope, status)
	}
	b.WriteString("\n")

	if len(d.Findings) == 0 {
		b.WriteString("  nothing to report.\n\n")
		return b.String()
	}
	for _, f := range d.Findings {
		marker := "i"
		switch f.Severity {
		case "error":
			marker = "x"
		case "warn":
			marker = "!"
		}
		fmt.Fprintf(b, "  %s %s\n", marker, f.Message)
		if f.Fix != "" {
			fmt.Fprintf(b, "    %s\n", f.Fix)
		}
		fmt.Fprintf(b, "    %s\n\n", f.Code)
	}
	if d.OK {
		b.WriteString("  nothing would stop this run.\n\n")
	} else {
		b.WriteString("  this configuration would not run as written.\n\n")
	}
	return b.String()
}
