package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/live"
	"github.com/IshaanNene/Tracepoint/internal/plan"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	clirender "github.com/IshaanNene/Tracepoint/internal/render/cli"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/session"
)

type runOptions struct {
	configPath   string
	from         string
	resultPath   string
	reportPath   string
	noLive       bool
	policyPath   string
	runRoot      string
	outDir       string
	events       string
	overrides    []string
	dryRun       bool
	detach       bool
	allowInvalid bool
	verbose      bool
	seed         uint64
	seedSet      bool
	thresholds   map[string]float64
}

func newRunCmd(env Env, g *globals) *cobra.Command {
	var (
		opts       runOptions
		thresholds = map[string]*time.Duration{}
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a load test and report which tier was slow",
		Long: strings.TrimSpace(`
Run the configuration, then report. Every run gets a directory - runs/<id>/ unless
--out-dir says otherwise - holding state.json, events.ndjson, result.json, digest.json, report.html,
config.effective.yaml and run.log.

Exit code 0 means the run was valid and met its budgets; 1 means a budget was breached;
3 means it failed or was cut short; 4 means the run itself cannot be believed, because
the load generator rather than the target set the pace.

With --detach, validation and preflight happen before the command returns, so a typo is
still reported at once; the load then runs in the background and the command prints
{run_id, run_dir}. Follow it with status, wait and stop.`),
		Example: strings.TrimSpace(`
  tracepoint run -c tracepoint.yaml
  tracepoint run -c tracepoint.yaml --output json > result.json
  tracepoint run -c tracepoint.yaml --detach --output json
  tracepoint run -c tracepoint.yaml --events - | jq -c 'select(.type == "bucket.sealed")'
  tracepoint run --from 20260924T100000Z-abc123 --set http.executor.max_in_flight=240
  cat tracepoint.yaml | tracepoint run -c -`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.seedSet = cmd.Flags().Changed("seed")
			opts.thresholds = thresholdFlags(cmd, thresholds)
			if opts.from != "" && cmd.Flags().Changed("config") {
				return errs.New(errs.CodeOpsInvalidInput, "--from and --config both name a configuration").
					WithHint("--from re-runs a previous run's configuration; use one or the other")
			}
			return runRun(cmd.Context(), env, g, opts)
		},
	}
	f := cmd.Flags()
	for _, name := range []string{"http", "db", "redis"} {
		thresholds[name] = f.Duration(name+"-threshold", 0,
			"hot-bucket threshold on "+name+" p99 service time when slo."+name+".p99 is not set (default 100ms)")
	}
	f.StringVarP(&opts.configPath, "config", "c", "tracepoint.yaml", "configuration file, or - to read standard input")
	f.StringVar(&opts.from, "from", "", "re-run a previous run's effective configuration, by run id or directory; combine with --set")
	f.StringVar(&opts.resultPath, "result-path", "", "also write result.json here")
	f.StringVar(&opts.reportPath, "report-path", "", "also write report.html here")
	f.StringVar(&opts.outDir, "out-dir", "", "write the run directory here instead of under the run root")
	f.StringVar(&opts.runRoot, "run-root", "", "directory run directories are created under; defaults to the policy's run_root, then runs/")
	f.StringVar(&opts.events, "events", "", "stream events as NDJSON: - for stdout (which then carries nothing else), or a file path")
	f.BoolVar(&opts.noLive, "no-live", false, "log a progress line every 5s instead of drawing the live view, even on a terminal")
	f.BoolVar(&opts.detach, "detach", false, "validate and preflight, then run in the background and print {run_id, run_dir}")
	f.BoolVar(&opts.allowInvalid, "allow-invalid", false, "exit 0 for a run the generator bottlenecked, instead of 4")
	f.BoolVarP(&opts.verbose, "verbose", "v", false, "include the per-label table")
	f.Uint64Var(&opts.seed, "seed", 0, "override the configured seed; the seed actually used is always recorded in the result")
	f.StringVar(&opts.policyPath, "policy", "", "safety policy file; defaults to $TRACEPOINT_POLICY, then to an envelope that refuses public targets and grants no writes")
	f.StringArrayVar(&opts.overrides, "set", nil, "override a configuration value, as path=value. Repeatable. List items are addressable by name, as in db.queries[by-id].weight=5")
	f.BoolVar(&opts.dryRun, "dry-run", false, "describe what would run - load, targets, effective safety envelope - and contact nothing")
	return cmd
}

func runRun(ctx context.Context, env Env, g *globals, opts runOptions) error {
	granted, err := policy.Load(opts.policyPath, env.Lookup)
	if err != nil {
		return err
	}
	store, err := openStore(env, granted, opts.runRoot)
	if err != nil {
		return err
	}

	// The run logs through a handler the live view can take over for the length of
	// the run and hand back.
	logs := newSwitchHandler(g.logger(env).Handler())
	req := session.Request{
		Overrides: opts.overrides, Granted: granted, Lookup: env.Lookup,
		Thresholds: opts.thresholds, AllowInvalid: opts.allowInvalid, Actor: env.actor(),
		Logger: slog.New(logs), Store: store, OutDir: opts.outDir, ResultPath: opts.resultPath, ReportPath: opts.reportPath,
		Detached: opts.detach,
	}
	if opts.seedSet {
		req.Seed = &opts.seed
	}
	if req.Source, req.SourcePath, err = readSource(env, store, opts.configPath, opts.from); err != nil {
		return err
	}

	// A dry run answers "what would this do" without doing any of it, which is what
	// makes it safe to point at a configuration nobody has read yet.
	if opts.dryRun {
		cfg, effective, warnings, lerr := session.Load(ctx, req)
		if lerr != nil {
			return lerr
		}
		p := plan.Build(cfg, effective, warnings, req.SourcePath, opts.overrides)
		if g.json() {
			return writeJSON(env.Stdout, p)
		}
		if _, werr := io.WriteString(env.Stdout, plan.Render(p)); werr != nil {
			return errs.Wrap(errs.CodeIOWriteFailed, werr, "writing the plan")
		}
		return nil
	}

	var closeEvents func()
	stdoutIsEvents := opts.events == "-"
	if opts.events != "" {
		w, closer, eerr := eventSink(env, opts.events)
		if eerr != nil {
			return eerr
		}
		closeEvents = closer
		req.EventWriters = append(req.EventWriters, w)
	}
	if closeEvents != nil {
		defer closeEvents()
	}
	// A person at a terminal gets the live view; everywhere else - a pipe, CI, an
	// agent - one progress line every five seconds.
	var view *live.View
	var plain *plainProgress
	showLive := env.IsTTY && env.ErrTTY && !env.CI && !opts.noLive && !g.json() && !stdoutIsEvents && !opts.detach
	switch {
	case showLive:
		view = live.Start(env.Stderr, g.colour(env))
		// From here until finishView the view owns stderr; a log line would tear it.
		logs.set(view.Handler())
		req.Progress = view.Progress
		req.EventFuncs = append(req.EventFuncs, view.Event)
	case !g.json() && !stdoutIsEvents && !opts.detach:
		plain = newPlainProgress(req.Logger)
		req.Progress = plain.observe
	}
	finishView := func(status string) {
		if view != nil {
			view.Finish(status)
			logs.set(g.logger(env).Handler())
			view = nil
		}
	}
	defer finishView("stopped")

	prepared, err := session.Prepare(ctx, req)
	if err != nil {
		return err
	}
	if view == nil {
		// The live view shows these from the event stream instead.
		for _, w := range prepared.Warnings {
			req.Logger.Warn(w.Message, "code", w.Code, "fix", w.Fix)
		}
	}
	if plain != nil {
		plain.setRunners(prepared.Config.Runners())
	}

	if opts.detach {
		return detach(ctx, env, g, prepared)
	}

	// The first signal is a graceful stop: arrivals cease, in-flight work drains, and
	// a partial result is written and marked interrupted. The second exits at once,
	// because someone pressing it twice wants out now.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	finished := make(chan struct{})
	defer close(finished)
	go watchForSecondSignal(ctx, finished, env)

	req.Logger.Info("starting", "run_id", prepared.ID(), "run_dir", prepared.Run.Dir,
		"duration", prepared.Config.Run.Duration.String(), "runners", prepared.Config.Runners())
	if view != nil {
		view.Run(prepared.ID(), prepared.Config.Run.Duration.D(), prepared.Config.Run.Timeout.D())
		// A message sent after the view has finished is dropped, so a late signal is
		// harmless.
		go func(v *live.View) {
			select {
			case <-ctx.Done():
				v.Stopping()
			case <-finished:
			}
		}(view)
	}

	out, err := prepared.Execute(ctx)
	if err != nil {
		finishView("failed")
		return err
	}
	finishView(out.Result.Run.Status)

	switch {
	case stdoutIsEvents:
		// stdout carries the event stream and nothing else; run.finished holds the
		// digest, so a reader of the stream needs no other document.
	case g.json():
		if err := writeJSON(env.Stdout, out.Result); err != nil {
			return err
		}
	default:
		if err := clirender.Render(env.Stdout, out.Result, clirender.Options{
			Colour: g.colour(env), Verbose: opts.verbose,
		}); err != nil {
			return err
		}
	}
	if out.ExitCode != errs.ExitOK {
		return &exitError{code: out.ExitCode, documented: true, err: out.Err}
	}
	return nil
}

// detach preflights, starts the background run and reports where it lives.
func detach(ctx context.Context, env Env, g *globals, p *session.Prepared) error {
	if err := p.Preflight(ctx); err != nil {
		return err
	}
	pid, err := p.Detach(ctx, env.Executable)
	if err != nil {
		return err
	}
	doc := map[string]any{"run_id": p.ID(), "run_dir": p.Run.Dir, "pid": pid, "status": runstore.StatusPending}
	if g.json() {
		return writeJSON(env.Stdout, doc)
	}
	fmt.Fprintf(env.Stdout, "started %s in the background\n  run dir  %s\n  follow   tracepoint wait %s\n", p.ID(), p.Run.Dir, p.ID())
	return nil
}

// readSource reads the configuration once, as written. With --from it is the stored
// effective configuration of an earlier run.
func readSource(env Env, store *runstore.Store, path, from string) ([]byte, string, error) {
	if from != "" {
		r, err := store.Get(from)
		if err != nil {
			return nil, "", err
		}
		p := r.Path(runstore.ConfigFile)
		b, err := os.ReadFile(p) //nolint:gosec // the run directory the user named
		if err != nil {
			return nil, "", errs.Wrap(errs.CodeConfigNotFound, err, "run %s has no stored configuration", r.ID).
				WithHint("it may predate the run store, or have failed before recording one")
		}
		return b, p, nil
	}
	if path == "-" {
		stdin := env.Stdin
		if stdin == nil {
			stdin = os.Stdin
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, "", errs.Wrap(errs.CodeIOReadFailed, err, "reading the configuration from standard input")
		}
		return b, "-", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // the configuration the user named
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", errs.Wrap(errs.CodeConfigNotFound, err, "no configuration at %s", path).
				WithHint("check the path, or pass -c - to read the configuration from standard input")
		}
		return nil, "", errs.Wrap(errs.CodeIOReadFailed, err, "reading %s", path)
	}
	return b, path, nil
}

// openStore opens the run root: the flag, then the injected root, then the policy's
// run_root, then runs/.
func openStore(env Env, granted policy.Policy, flag string) (*runstore.Store, error) {
	root := flag
	if root == "" {
		root = env.RunRoot
	}
	if root == "" {
		root = granted.RunRoot
	}
	return runstore.Open(root, nil)
}

func eventSink(env Env, target string) (io.Writer, func(), error) {
	if target == "-" {
		return env.Stdout, nil, nil
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the path the user named
	if err != nil {
		return nil, nil, errs.Wrap(errs.CodeIOWriteFailed, err, "opening %s for events", target)
	}
	return f, func() { _ = f.Close() }, nil
}

// prepare loads the configuration and the policy, applies overrides, and resolves the
// envelope the run will execute under. Nothing is contacted.
func prepare(ctx context.Context, env Env, configPath, policyPath string, overrides []string) (*config.Config, policy.Policy, []config.Warning, error) {
	granted, err := policy.Load(policyPath, env.Lookup)
	if err != nil {
		return nil, policy.Policy{}, nil, err
	}
	src, srcPath, err := readSource(env, nil, configPath, "")
	if err != nil {
		return nil, policy.Policy{}, nil, err
	}
	return session.Load(ctx, session.Request{
		Source: src, SourcePath: srcPath, Overrides: overrides, Granted: granted, Lookup: env.Lookup,
	})
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
