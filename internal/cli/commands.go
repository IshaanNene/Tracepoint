package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/engine"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	clirender "github.com/IshaanNene/Tracepoint/internal/render/cli"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
)

func newRunCmd(env Env, g *globals) *cobra.Command {
	var (
		configPath   string
		resultPath   string
		policyPath   string
		overrides    []string
		dryRun       bool
		allowInvalid bool
		verbose      bool
		seed         uint64
		seedSet      bool
		thresholds   = map[string]*time.Duration{}
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a load test and report which tier was slow",
		Long: strings.TrimSpace(`
Run the configuration, then report. Exit code 0 means the run was valid and met its
budgets; 1 means a budget was breached; 4 means the run itself cannot be believed,
because the load generator rather than the target set the pace.`),
		Example: strings.TrimSpace(`
  tracepoint run -c tracepoint.yaml
  tracepoint run -c tracepoint.yaml --output json > result.json
  cat tracepoint.yaml | tracepoint run -c -`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			seedSet = cmd.Flags().Changed("seed")
			return runRun(cmd.Context(), env, g, runOptions{
				configPath: configPath, resultPath: resultPath, policyPath: policyPath,
				overrides: overrides, dryRun: dryRun,
				allowInvalid: allowInvalid, verbose: verbose,
				seed: seed, seedSet: seedSet,
				thresholds: thresholdFlags(cmd, thresholds),
			})
		},
	}
	f := cmd.Flags()
	for _, name := range []string{"http", "db", "redis"} {
		thresholds[name] = f.Duration(name+"-threshold", 0,
			"hot-bucket threshold on "+name+" p99 service time when slo."+name+".p99 is not set (default 100ms)")
	}
	f.StringVarP(&configPath, "config", "c", "tracepoint.yaml", "configuration file, or - to read standard input")
	f.StringVar(&resultPath, "result-path", "", "also write result.json here")
	f.BoolVar(&allowInvalid, "allow-invalid", false, "exit 0 for a run the generator bottlenecked, instead of 4")
	f.BoolVarP(&verbose, "verbose", "v", false, "include the per-label table")
	f.Uint64Var(&seed, "seed", 0, "override the configured seed; the seed actually used is always recorded in the result")
	f.StringVar(&policyPath, "policy", "", "safety policy file; defaults to $TRACEPOINT_POLICY, then to an envelope that refuses public targets and grants no writes")
	f.StringArrayVar(&overrides, "set", nil, "override a configuration value, as path=value. Repeatable. List items are addressable by name, as in db.queries[by-id].weight=5")
	f.BoolVar(&dryRun, "dry-run", false, "describe what would run - load, targets, effective safety envelope - and contact nothing")
	return cmd
}

// watchForSecondSignal turns a second interrupt into an immediate exit. It returns as
// soon as the command finishes, so it never outlives the run it belongs to.
func watchForSecondSignal(ctx context.Context, finished <-chan struct{}, env Env) {
	select {
	case <-ctx.Done(): // the first signal arrived, or the command finished
	case <-finished:
		return
	}
	second := make(chan os.Signal, 1)
	signal.Notify(second, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(second)

	select {
	case <-second:
		fmt.Fprintln(env.Stderr, "\ntracepoint: interrupted again, exiting now")
		env.exit(errs.ExitRuntime)
	case <-finished:
	}
}

type runOptions struct {
	configPath   string
	resultPath   string
	policyPath   string
	overrides    []string
	dryRun       bool
	allowInvalid bool
	verbose      bool
	seed         uint64
	seedSet      bool
	thresholds   map[string]float64
}

// thresholdFlags collects the --<runner>-threshold flags that were actually given, in
// milliseconds.
func thresholdFlags(cmd *cobra.Command, flags map[string]*time.Duration) map[string]float64 {
	out := map[string]float64{}
	for name, d := range flags {
		if cmd.Flags().Changed(name+"-threshold") && *d > 0 {
			out[name] = float64(*d) / float64(time.Millisecond)
		}
	}
	return out
}

func runRun(ctx context.Context, env Env, g *globals, opts runOptions) error {
	cfg, effective, warnings, err := prepare(ctx, env, opts.configPath, opts.policyPath, opts.overrides)
	if err != nil {
		return err
	}

	// A dry run answers "what would this do" without doing any of it, which is what
	// makes it safe to point at a configuration nobody has read yet.
	if opts.dryRun {
		plan := BuildPlan(cfg, effective, warnings, opts.configPath, opts.overrides)
		if g.json() {
			return writeJSON(env.Stdout, plan)
		}
		_, writeErr := io.WriteString(env.Stdout, RenderPlan(plan))
		if writeErr != nil {
			return errs.Wrap(errs.CodeIOWriteFailed, writeErr, "writing the plan")
		}
		return nil
	}

	log := g.logger(env)
	for _, w := range warnings {
		log.Warn(w.Message, "code", w.Code, "fix", w.Fix)
	}
	eopts := engine.Options{
		Config: cfg, SourcePath: opts.configPath, Clock: nil, Logger: log,
		Overrides: opts.overrides, Policy: effective,
		Warnings:   planWarnings(warnings),
		Thresholds: opts.thresholds,
	}
	if opts.seedSet {
		eopts.Seed = &opts.seed
	}
	if !g.json() {
		eopts.Progress = progressPrinter(env, g)
	}

	eng, err := engine.New(eopts)
	if err != nil {
		return err
	}

	// The first signal is a graceful stop: arrivals cease, in-flight work drains, and
	// a partial result is written and marked interrupted. The second exits at once,
	// because someone pressing it twice wants out now and does not care about the
	// partial result.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The watcher has to be able to finish. Without the done channel it would sit
	// waiting for a second signal that never comes, for the lifetime of the process -
	// harmless in a one-shot command, a leak everywhere the engine is embedded.
	finished := make(chan struct{})
	defer close(finished)
	go watchForSecondSignal(ctx, finished, env)

	log.Info("starting", "run_id", eng.RunID(), "seed", eng.Seed(),
		"duration", cfg.Run.Duration.String(), "runners", cfg.Runners())

	res, err := eng.Run(ctx)
	if err != nil {
		return err
	}

	if opts.resultPath != "" {
		if err := writeResultFile(opts.resultPath, res); err != nil {
			return err
		}
		res.Artifacts = &result.Artifacts{Result: opts.resultPath}
	}

	if g.json() {
		if err := writeJSON(env.Stdout, res); err != nil {
			return err
		}
	} else if err := clirender.Render(env.Stdout, res, clirender.Options{
		Colour: g.colour(env), Verbose: opts.verbose,
	}); err != nil {
		return err
	}

	return outcomeExit(res, opts.allowInvalid)
}

// prepare loads the configuration and the policy, applies overrides, and resolves the
// envelope the run will execute under.
//
// Everything here happens before anything is contacted, so a refusal costs nothing and
// a typo is reported in a second.
func prepare(ctx context.Context, env Env, configPath, policyPath string, overrides []string) (*config.Config, policy.Policy, []config.Warning, error) {
	granted, err := policy.Load(policyPath, env.Lookup)
	if err != nil {
		return nil, policy.Policy{}, nil, err
	}
	// Overrides are applied to the document as written, before defaults are derived.
	// They sit at the top of the precedence chain, and a default computed from a value
	// an override is about to change would contradict it: raising run.duration has to
	// lengthen the stage the `rate:` shorthand produces, not leave it at the old value.
	cfg, err := config.Load(ctx, config.FromFile(configPath), config.Options{
		Lookup: env.Lookup, Stdin: env.Stdin, Raw: len(overrides) > 0,
	})
	if err != nil {
		return nil, policy.Policy{}, nil, err
	}
	if len(overrides) > 0 {
		if setErr := cfg.ApplySet(overrides); setErr != nil {
			return nil, policy.Policy{}, nil, setErr
		}
		// An override can break a configuration as easily as fix one, so the whole
		// thing is defaulted and validated afresh.
		if finalErr := cfg.Finalise(); finalErr != nil {
			return nil, policy.Policy{}, nil, finalErr
		}
	}
	effective, warnings, err := cfg.ApplyPolicy(granted)
	if err != nil {
		return nil, policy.Policy{}, nil, err
	}
	return cfg, effective, warnings, nil
}

func planWarnings(in []config.Warning) []result.Finding {
	out := make([]result.Finding, 0, len(in))
	for _, w := range in {
		out = append(out, result.Finding{
			Code: w.Code, Severity: result.SeverityWarn, Message: w.Message, Fix: w.Fix,
		})
	}
	return out
}

// outcomeExit turns a completed run into an exit code.
//
// A run that finished is not necessarily a run that passed, and the two failures mean
// different things: 4 says the measurement cannot be believed, 1 says the measurement
// is sound and the target missed its budget. Validity is checked first because an
// invalid run's budget result is meaningless.
func outcomeExit(res *result.Result, allowInvalid bool) error {
	if res.Analysis.Validity.State == result.ValidityInvalid && !allowInvalid {
		return &exitError{code: errs.ExitInvalid, documented: true, err: errs.New(errs.CodeRunInvalid,
			"the load generator, not the target, set the pace, so this run says nothing about the target").
			WithHint("see the findings above; pass --allow-invalid to exit 0 anyway")}
	}
	if !res.Analysis.SLO.Pass {
		return &exitError{code: errs.ExitBreach, documented: true, err: errs.New(errs.CodeSLOBreach,
			"the target missed a configured budget")}
	}
	return nil
}

func writeResultFile(path string, res *result.Result) error {
	data, err := res.Encode()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return errs.Wrap(errs.CodeIOWriteFailed, err, "creating %s", dir)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return errs.Wrap(errs.CodeIOWriteFailed, err, "writing %s", path)
	}
	return nil
}

// progressPrinter writes one line per second to stderr. The live terminal view arrives
// in phase 5; until then this is the same information without the redraw, and it is
// what a non-terminal caller would get anyway.
func progressPrinter(env Env, g *globals) func(engine.Progress) {
	log := g.logger(env)
	return func(p engine.Progress) {
		log.Info("progress",
			"runner", p.Runner,
			"elapsed", p.Elapsed.Round(time.Second).String(),
			"of", p.Total.String(),
			"offered", p.Offered, "done", p.Done, "errors", p.Errors,
			"in_flight", p.InFlight, "p99_ms", fmt.Sprintf("%.1f", p.P99MS))
	}
}

func newValidateCmd(env Env, g *globals) *cobra.Command {
	var (
		configPath string
		policyPath string
		overrides  []string
	)

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check a configuration without running anything",
		Long: strings.TrimSpace(`
Load, interpolate and validate a configuration, reporting every problem it has rather
than only the first. Nothing is contacted and no load is generated, so this is safe to
run against a production configuration.`),
		Example: strings.TrimSpace(`
  tracepoint validate -c tracepoint.yaml
  tracepoint validate -c tracepoint.yaml --output json`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, warnings, err := prepare(cmd.Context(), env, configPath, policyPath, overrides)
			if err != nil {
				return err
			}
			if g.json() {
				return writeJSON(env.Stdout, map[string]any{
					"valid":     true,
					"path":      configPath,
					"runners":   cfg.Runners(),
					"duration":  cfg.Run.Duration.String(),
					"targets":   cfg.Targets(),
					"warnings":  warnings,
					"effective": cfg.Redacted(),
				})
			}
			fmt.Fprintf(env.Stdout, "%s is valid: %s over %s\n",
				configPath, strings.Join(cfg.Runners(), ", "), cfg.Run.Duration)
			for _, w := range warnings {
				fmt.Fprintf(env.Stdout, "  ! %s\n", w.Message)
				if w.Fix != "" {
					fmt.Fprintf(env.Stdout, "    %s\n", w.Fix)
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&configPath, "config", "c", "tracepoint.yaml", "configuration file, or - to read standard input")
	f.StringVar(&policyPath, "policy", "", "safety policy file to validate against")
	f.StringArrayVar(&overrides, "set", nil, "override a configuration value, as path=value")
	return cmd
}

func newSchemaCmd(env Env) *cobra.Command {
	names := make([]string, 0, len(schemas.Names()))
	lines := make([]string, 0, len(schemas.Names()))
	for _, n := range schemas.Names() {
		names = append(names, string(n))
		lines = append(lines, fmt.Sprintf("  %-8s %s", n, schemas.Describe(n)))
	}

	return &cobra.Command{
		Use:   "schema <" + strings.Join(names, "|") + ">",
		Short: "Print a JSON Schema for one of TracePoint's contracts",
		Long: strings.TrimSpace(`
Print the JSON Schema for one of TracePoint's contract documents. The schemas are
embedded in the binary, so what is printed is exactly what this build produces and
validates against - they cannot disagree.

` + strings.Join(lines, "\n")),
		Example: strings.TrimSpace(`
  tracepoint schema config > config.schema.json
  tracepoint schema result | jq '.properties.analysis'`),
		Args:      cobra.ExactArgs(1),
		ValidArgs: names,
		RunE: func(_ *cobra.Command, args []string) error {
			doc, err := schemas.Get(schemas.Name(args[0]))
			if err != nil {
				return err
			}
			if _, err := env.Stdout.Write(doc); err != nil {
				return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the schema")
			}
			return nil
		},
	}
}

func newVersionCmd(env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the version of this build",
		Args:    cobra.NoArgs,
		Example: "  tracepoint version --output json",
		RunE: func(*cobra.Command, []string) error {
			info := buildinfo.Get()
			if g.json() {
				return writeJSON(env.Stdout, info)
			}
			fmt.Fprintf(env.Stdout, "tracepoint %s (%s, built %s, %s %s/%s)\n",
				info.Version, info.Commit, info.Date, info.Go, info.OS, info.Arch)
			return nil
		},
	}
}
