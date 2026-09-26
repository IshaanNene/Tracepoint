// Package session runs one test from configuration to finished run directory: load and
// validate, create the run directory, record the effective configuration, run the
// engine, write the result and the digest, stream events, keep state.json current and
// append to the audit log.
//
// It is the one path a run takes, whoever asked for it. The foreground CLI, a detached
// background run and a run started by the MCP or REST server all go through here, so
// there is exactly one definition of "run a test and decide how it went" - which is
// what the adapters must not each grow their own copy of (ADR-009).
package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/engine"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/events"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	htmlreport "github.com/IshaanNene/Tracepoint/internal/render/html"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
)

// Request is everything a run needs.
type Request struct {
	// Source is the configuration as written; SourcePath says where it came from.
	Source     []byte
	SourcePath string
	Overrides  []string
	// Granted is the policy a human granted. The configuration can only tighten it.
	Granted policy.Policy
	Lookup  func(string) (string, bool)

	Seed         *uint64
	Thresholds   map[string]float64
	AllowInvalid bool
	Actor        string

	Clock    clock.Clock
	Logger   *slog.Logger
	Progress func(engine.Progress)
	// EventWriters and EventFuncs receive the event stream besides events.ndjson.
	EventWriters []io.Writer
	EventFuncs   []func(events.Event)

	Store *runstore.Store
	// OutDir, when set, is the run directory itself rather than one under the store.
	OutDir string
	// RunID continues a run whose directory already exists - a detached run's child.
	RunID string
	// ResultPath, when set, receives a copy of result.json.
	ResultPath string
	// ReportPath, when set, receives a copy of report.html.
	ReportPath string
	Detached   bool
}

// Load reads the configuration and the policy, applies overrides and resolves the
// envelope the run executes under. Nothing is contacted.
func Load(ctx context.Context, req Request) (*config.Config, policy.Policy, []config.Warning, error) {
	// Overrides are applied to the document as written, before defaults are derived:
	// they are at the top of the precedence chain, and a default computed from a value
	// an override is about to change would contradict it.
	cfg, err := config.Load(ctx, config.FromBytes(req.SourcePath, req.Source), config.Options{
		Lookup: req.Lookup, Raw: len(req.Overrides) > 0,
	})
	if err != nil {
		return nil, policy.Policy{}, nil, err
	}
	if len(req.Overrides) > 0 {
		if e := cfg.ApplySet(req.Overrides); e != nil {
			return nil, policy.Policy{}, nil, e
		}
		if e := cfg.Finalise(); e != nil {
			return nil, policy.Policy{}, nil, e
		}
	}
	// A stored configuration keeps no secret it was given inline. A re-run that still
	// holds the marker would send it to the target as a password.
	if paths := cfg.RedactedPaths(); len(paths) > 0 {
		return nil, policy.Policy{}, nil, errs.New(errs.CodeConfigInvalidValue,
			"the configuration holds %d redacted secret(s) that must be supplied again", len(paths)).
			WithPath(paths[0]).
			WithHint("supply each with --set %s=... or an environment reference; redacted: %v", paths[0], paths)
	}
	effective, warnings, err := cfg.ApplyPolicy(req.Granted)
	if err != nil {
		return nil, policy.Policy{}, nil, err
	}
	return cfg, effective, append(warnings, cfg.Caveats()...), nil
}

// Prepared is a validated run with a directory on disk.
type Prepared struct {
	req      Request
	Config   *config.Config
	Policy   policy.Policy
	Warnings []config.Warning
	Run      *runstore.Run
	seed     uint64
	hash     string
	clk      clock.Clock
	log      *slog.Logger
}

// ID is the run's identifier.
func (p *Prepared) ID() string { return p.Run.ID }

// Prepare validates the request and creates the run directory, with state pending
// and the effective configuration written. It refuses a run the policy's
// max_concurrent_runs would exceed: two tests against one target measure each other.
func Prepare(ctx context.Context, req Request) (*Prepared, error) {
	if req.Clock == nil {
		req.Clock = clock.New()
	}
	if req.Logger == nil {
		req.Logger = slog.New(slog.DiscardHandler)
	}
	if req.Store == nil {
		return nil, errs.New(errs.CodeInternal, "a session needs a run store")
	}
	cfg, eff, warnings, err := Load(ctx, req)
	if err != nil {
		return nil, err
	}

	p := &Prepared{req: req, Config: cfg, Policy: eff, Warnings: warnings, clk: req.Clock}
	switch {
	case req.Seed != nil:
		p.seed = *req.Seed
	case cfg.Run.Seed != nil:
		p.seed = *cfg.Run.Seed
	default:
		p.seed = drawSeed()
	}
	if p.hash, err = result.ConfigHash(cfg.Redacted()); err != nil {
		return nil, err
	}

	if req.RunID != "" {
		// A detached run's child continues the directory its parent created.
		r, gerr := req.Store.Get(firstNonEmpty(req.OutDir, req.RunID))
		if gerr != nil {
			return nil, gerr
		}
		p.Run = r
	} else {
		id := engine.NewRunID(req.Clock.Now(), p.seed)
		r, err := req.Store.CreateLimited(id, req.OutDir, eff.MaxConcurrentRuns)
		if err != nil {
			return nil, err
		}
		p.Run = r
		doc, _, yerr := config.EffectiveYAML(ctx, req.Source, req.Overrides, req.Lookup)
		if yerr != nil {
			return nil, yerr
		}
		if err := r.WriteFile(runstore.ConfigFile, doc); err != nil {
			return nil, err
		}
		if err := r.UpdateState(func(st *runstore.State) {
			st.Actor, st.ConfigHash, st.Detached = req.Actor, p.hash, req.Detached
		}); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Preflight checks targets and connectivity without generating load, then releases
// everything it opened.
func (p *Prepared) Preflight(ctx context.Context) error {
	eng, err := p.engine(nil, nil)
	if err != nil {
		return err
	}
	if err := eng.Preflight(ctx); err != nil {
		p.fail(err, "preflight")
		return err
	}
	return nil
}

// Outcome is how a finished run went.
type Outcome struct {
	Result *result.Result
	Digest *result.Digest
	// ExitCode is what the process should exit with; Err explains a non-zero one and
	// is already described in the result, so it is never printed as a second document.
	ExitCode int
	Err      error
}

// stopPoll is how often a run checks for the STOP sentinel.
const stopPoll = 500 * time.Millisecond

// Execute runs the test and records everything it produced. A returned error means no
// result could be produced at all; a run that finished badly returns an Outcome.
func (p *Prepared) Execute(ctx context.Context) (*Outcome, error) {
	r := p.Run
	logFile, err := os.OpenFile(r.Path(runstore.LogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.CodeIOWriteFailed, err, "opening the run log")
	}
	defer func() { _ = logFile.Close() }()
	p.log = slog.New(teeHandler{p.req.Logger.Handler(), slog.NewJSONHandler(logFile, nil)}).With("run_id", r.ID)

	eventsFile, err := r.OpenEvents()
	if err != nil {
		return nil, err
	}
	defer func() { _ = eventsFile.Close() }()
	emitter := events.NewEmitter(r.ID, p.clk)
	emitter.AddWriter(eventsFile)
	for _, w := range p.req.EventWriters {
		emitter.AddWriter(w)
	}
	for _, f := range p.req.EventFuncs {
		emitter.AddFunc(f)
	}

	now := p.clk.Now().UTC().Format(time.RFC3339Nano)
	if e := r.UpdateState(func(st *runstore.State) {
		st.Status, st.Phase, st.PID, st.StartedAt, st.Heartbeat = runstore.StatusRunning, "preflight", os.Getpid(), now, now
		st.Actor, st.ConfigHash, st.Detached = firstNonEmpty(st.Actor, p.req.Actor), p.hash, st.Detached || p.req.Detached
	}); e != nil {
		return nil, e
	}
	p.audit(runstore.AuditRecord{Event: "started"})
	p.log.Info("run started", "config_hash", p.hash, "seed", p.seed, "actor", p.req.Actor, "detached", p.req.Detached)

	// A stop request is a graceful stop with a stated reason, exactly like a signal.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopWatch := p.watchStop(ctx, cancel)
	defer stopWatch()

	// Progress is written by the engine's sweeper and read by the heartbeat.
	var (
		progressMu sync.Mutex
		progress   runstore.Progress
	)
	total := float64(p.Config.Run.Duration.D()) / float64(time.Millisecond)
	progressFn := func(pr engine.Progress) {
		progressMu.Lock()
		progress.ElapsedMS, progress.TotalMS = float64(pr.Elapsed)/float64(time.Millisecond), total
		if total > 0 {
			progress.Ratio = min(1, progress.ElapsedMS/total)
		}
		progressMu.Unlock()
		if p.req.Progress != nil {
			p.req.Progress(pr)
		}
	}
	stopBeat := r.Heartbeat(context.WithoutCancel(ctx), func() *runstore.Progress {
		progressMu.Lock()
		defer progressMu.Unlock()
		cp := progress
		return &cp
	})

	eng, err := p.engine(emitter, progressFn)
	if err != nil {
		stopBeat()
		p.fail(err, "preflight")
		return nil, err
	}
	p.update(func(st *runstore.State) { st.Phase = "load" })

	res, err := eng.Run(ctx)
	stopBeat()
	if err != nil {
		p.fail(err, "load")
		emitter.Emit(events.RunFinished, map[string]any{
			"status": runstore.StatusFailed, "exit_code": errs.ExitCodeOf(err), "error": errs.EnvelopeOf(err),
		})
		return nil, err
	}
	return p.finish(res, emitter)
}

// update records a state change. A run whose bookkeeping cannot be written is still a
// run worth finishing, so a failure is logged rather than returned.
func (p *Prepared) update(change func(*runstore.State)) {
	if err := p.Run.UpdateState(change); err != nil {
		p.logger().Warn("the run state could not be updated", "err", err)
	}
}

// engine builds the engine for this run.
func (p *Prepared) engine(em *events.Emitter, progress func(engine.Progress)) (*engine.Engine, error) {
	seed := p.seed
	return engine.New(engine.Options{
		Config: p.Config, SourcePath: p.req.SourcePath, Overrides: p.req.Overrides,
		Clock: p.clk, Logger: p.logger(), RunID: p.Run.ID, Seed: &seed,
		Policy: p.Policy, Warnings: planWarnings(p.Warnings),
		Thresholds: p.req.Thresholds, Events: em, Progress: progress,
	})
}

func (p *Prepared) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return p.req.Logger
}

// watchStop polls for the STOP sentinel and turns it into a graceful stop.
func (p *Prepared) watchStop(ctx context.Context, cancel context.CancelCauseFunc) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := p.clk.NewTicker(stopPoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C():
				if p.Run.StopRequested() {
					p.logger().Info("stop requested")
					cancel(errors.New("stop requested"))
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// finish writes the artifacts, streams what the analysis found and records the outcome.
func (p *Prepared) finish(res *result.Result, em *events.Emitter) (*Outcome, error) {
	r := p.Run
	p.update(func(st *runstore.State) { st.Phase = "analysis" })
	res.Artifacts = &result.Artifacts{
		RunDir: r.Dir, Result: r.Path(runstore.ResultFile), Digest: r.Path(runstore.DigestFile),
		Report: r.Path(runstore.ReportFile), Events: r.Path(runstore.EventsFile), Config: r.Path(runstore.ConfigFile), Log: r.Path(runstore.LogFile),
	}

	code, exitErr := ExitFor(res, p.req.AllowInvalid)
	digest := result.BuildDigest(res, result.DigestOptions{Ref: r.ID})

	for _, f := range append(append([]result.Finding(nil), res.Warnings...), res.Analysis.Validity.Findings...) {
		em.Emit(events.Warning, map[string]any{"code": f.Code, "message": f.Message, "severity": f.Severity})
	}
	for _, inc := range res.Analysis.Incidents {
		var hot []string
		for _, rn := range inc.Runners {
			if rn.Hot {
				hot = append(hot, rn.Name)
			}
		}
		var culprit any
		if inc.Culprit != nil && len(inc.Culprit.TiedWith) == 0 {
			culprit = inc.Culprit.Runner
		}
		em.Emit(events.IncidentOpened, map[string]any{"incident_id": inc.ID, "class": inc.Class, "start_s": inc.StartS, "runners_hot": hot})
		em.Emit(events.IncidentClosed, map[string]any{"incident_id": inc.ID, "class": inc.Class, "start_s": inc.StartS,
			"end_s": inc.EndS, "runners_hot": hot, "culprit": culprit})
	}

	raw, err := res.Encode()
	if err != nil {
		return nil, err
	}
	if e := r.WriteFile(runstore.ResultFile, raw); e != nil {
		return nil, e
	}
	draw, err := digest.Encode()
	if err != nil {
		return nil, err
	}
	if err := r.WriteFile(runstore.DigestFile, draw); err != nil {
		return nil, err
	}
	if p.req.ResultPath != "" {
		if err := writeFile(p.req.ResultPath, raw); err != nil {
			return nil, err
		}
	}
	p.writeReport(res)

	em.Emit(events.RunFinished, map[string]any{"status": res.Run.Status, "exit_code": code, "digest": digest})

	status := res.Run.Status
	finished := p.clk.Now().UTC().Format(time.RFC3339Nano)
	if err := r.UpdateState(func(st *runstore.State) {
		st.Status, st.Phase, st.FinishedAt, st.Heartbeat = status, "done", finished, finished
		st.ExitCode = &code
		if st.Progress != nil {
			st.Progress.Ratio = 1
		}
	}); err != nil {
		return nil, err
	}
	p.audit(runstore.AuditRecord{
		Event: "finished", Status: status, ExitCode: &code,
		Validity: res.Analysis.Validity.State, Bottleneck: res.Analysis.Verdict.Bottleneck,
	})
	if err := em.Err(); err != nil {
		p.logger().Warn("the event stream could not be written in full", "err", err)
	}
	p.logger().Info("run finished", "status", status, "exit_code", code,
		"validity", res.Analysis.Validity.State, "bottleneck", res.Analysis.Verdict.Bottleneck)
	return &Outcome{Result: res, Digest: digest, ExitCode: code, Err: exitErr}, nil
}

// writeReport renders report.html. The report is derived from result.json, which is
// already written, so a failure here costs the human view but not the run: it is
// logged, and `tracepoint report` can render it again.
func (p *Prepared) writeReport(res *result.Result) {
	page, err := htmlreport.Render(res, htmlreport.Options{})
	if err == nil {
		err = p.Run.WriteFile(runstore.ReportFile, page)
	}
	if err == nil && p.req.ReportPath != "" {
		err = writeFile(p.req.ReportPath, page)
	}
	if err != nil {
		p.logger().Warn("the HTML report could not be written; `tracepoint report` can render it from result.json", "err", err)
	}
}

// fail records a run that could not produce a result.
func (p *Prepared) fail(err error, phase string) {
	code := errs.ExitCodeOf(err)
	now := p.clk.Now().UTC().Format(time.RFC3339Nano)
	p.update(func(st *runstore.State) {
		st.Status, st.Phase, st.FinishedAt, st.Heartbeat = runstore.StatusFailed, phase, now, now
		st.ExitCode = &code
		st.Error = errs.EnvelopeOf(err)
	})
	p.audit(runstore.AuditRecord{Event: "failed", Status: runstore.StatusFailed, ExitCode: &code, Error: string(errs.EnvelopeOf(err).Error.Code)})
}

func (p *Prepared) audit(rec runstore.AuditRecord) {
	rec.RunID, rec.Actor, rec.ConfigHash = p.Run.ID, p.req.Actor, p.hash
	rec.Targets = p.Config.Targets()
	rec.PeakRates = PeakRates(p.Config)
	if err := p.req.Store.Audit(rec); err != nil {
		p.logger().Warn("the audit log could not be written", "err", err)
	}
}

// PeakRates is the highest rate, or VU count, each runner's profile reaches.
func PeakRates(cfg *config.Config) map[string]float64 {
	out := map[string]float64{}
	for _, name := range cfg.Runners() {
		ex := cfg.Executor(name)
		if ex == nil {
			continue
		}
		peak := ex.Rate
		if float64(ex.VUs) > peak {
			peak = float64(ex.VUs)
		}
		for _, s := range ex.Stages {
			if s.Target > peak {
				peak = s.Target
			}
		}
		out[name] = peak
	}
	return out
}

// ExitFor turns a finished run into an exit code (§6.1), with precedence 3 > 4 > 1:
// a run that was cut short, then one that cannot be believed, then a budget missed.
func ExitFor(res *result.Result, allowInvalid bool) (int, error) {
	switch res.Run.Status {
	case result.StatusAborted:
		return errs.ExitRuntime, errs.New(errs.CodeRunAborted, "the abort guard stopped the run: %s", res.Run.InterruptedReason).
			WithHint("the target was failing; the partial result is in the run directory")
	case result.StatusInterrupted:
		return errs.ExitRuntime, errs.New(errs.CodeRunInterrupted, "the run ended early: %s", res.Run.InterruptedReason).
			WithHint("the partial result is in the run directory")
	}
	if res.Analysis.Validity.State == result.ValidityInvalid && !allowInvalid {
		return errs.ExitInvalid, errs.New(errs.CodeRunInvalid,
			"the load generator, not the target, set the pace, so this run says nothing about the target").
			WithHint("see the findings; pass --allow-invalid to exit 0 anyway")
	}
	if !res.Analysis.SLO.Pass {
		return errs.ExitBreach, errs.New(errs.CodeSLOBreach, "the target missed a configured budget")
	}
	return errs.ExitOK, nil
}

func planWarnings(in []config.Warning) []result.Finding {
	out := make([]result.Finding, 0, len(in))
	for _, w := range in {
		severity := w.Severity
		if severity == "" {
			severity = result.SeverityWarn
		}
		out = append(out, result.Finding{Code: w.Code, Severity: severity, Message: w.Message, Fix: w.Fix})
	}
	return out
}

func writeFile(path string, data []byte) error {
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
