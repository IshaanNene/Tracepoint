package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/plan"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/session"
)

// MaxWait bounds one wait_for_run call, so an agent's tool call returns before the
// agent's own deadline and the agent polls in a loop it controls (ADR-008).
const MaxWait = 50

// ---- get_policy ----

// Empty is an operation that takes no input.
type Empty struct{}

// PolicyOut is the envelope this server enforces.
type PolicyOut struct {
	Policy policy.Policy `json:"policy" jsonschema:"The safety envelope every call is held to. A configuration can tighten it but never exceed it."`
	Change string        `json:"how_to_change" jsonschema:"Who can change it and how. An agent cannot."`
}

func (o *PolicyOut) summary() string {
	return fmt.Sprintf("policy: writes %v, public targets %v, %d concurrent run(s)", o.Policy.AllowWrites, o.Policy.AllowPublicTargets, o.Policy.MaxConcurrentRuns)
}

func getPolicy() Operation {
	return define(Operation{
		Name:  "get_policy",
		Title: "Get the safety policy",
		Description: "Return the safety policy this server enforces: which targets may be loaded, whether writes and destructive statements are allowed, rate, concurrency and duration ceilings, and how many runs may be active at once. " +
			"Call it before writing a configuration so the configuration fits, and when a call is refused with a POLICY_* code. The policy was fixed by a human when the server started; no call can change or exceed it - report a refusal to the human rather than working around it.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
	}, func(_ context.Context, s *Service, _ *Empty) (*PolicyOut, error) {
		return &PolicyOut{Policy: s.Policy,
			Change: "a human restarts the server with a different --policy file; no operation can change it"}, nil
	}, nil)
}

// ---- scaffold_config ----

// ScaffoldIn describes the system to write a starter configuration for.
type ScaffoldIn struct {
	BaseURL      string   `json:"base_url" jsonschema:"Base URL of the application under test, such as http://127.0.0.1:8080. Use an environment reference like ${BASE_URL} to keep hosts out of the file."`
	Paths        []string `json:"paths,omitempty" jsonschema:"Request paths to load, such as /api/items. Safe (read-only) endpoints only unless a human has said writes are fine. Defaults to /."`
	DBDriver     string   `json:"db_driver,omitempty" jsonschema:"Database to probe alongside: postgres, mysql or sqlite. Omit for none."`
	DBDSNEnv     string   `json:"db_dsn_env,omitempty" jsonschema:"Name of the environment variable holding the database DSN, such as DATABASE_URL. The file references it; the value is never written."`
	RedisAddrEnv string   `json:"redis_addr_env,omitempty" jsonschema:"Name of the environment variable holding the Redis address, such as REDIS_ADDR. Omit for no Redis probe."`
	Rate         float64  `json:"rate,omitempty" jsonschema:"Application requests per second. Defaults to 20; start low and raise it once a smoke run is valid."`
	Duration     string   `json:"duration,omitempty" jsonschema:"Run length as a Go duration, such as 30s or 3m. Defaults to 30s."`
	DBQuery      string   `json:"db_query,omitempty" jsonschema:"The database probe's query. Defaults to SELECT 1, which sees the connection but none of the application's tables; a read the application itself performs is a far better probe."`
}

// ScaffoldOut is a starter configuration.
type ScaffoldOut struct {
	ConfigYAML string   `json:"config_yaml" jsonschema:"A commented starter configuration. Pass it to validate_config, then plan_run, before start_run."`
	Notes      []string `json:"notes,omitempty" jsonschema:"What the starter assumed and what to check."`
}

func (o *ScaffoldOut) summary() string { return "wrote a starter configuration; validate it next" }

func scaffoldConfig() Operation {
	return define(Operation{
		Name:  "scaffold_config",
		Title: "Write a starter configuration",
		Description: "Write a commented starter configuration for an application and, optionally, the database and Redis it uses, probing each storage tier alongside the application so a slowdown can be attributed. " +
			"Use it when there is no configuration yet. It contacts nothing. Ask the human which targets and environments are allowed and what the SLOs are before running anything it produces.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "init",
	}, func(_ context.Context, _ *Service, in *ScaffoldIn) (*ScaffoldOut, error) {
		yaml, notes, err := Scaffold(*in)
		if err != nil {
			return nil, err
		}
		return &ScaffoldOut{ConfigYAML: yaml, Notes: notes}, nil
	}, func(in, _ *jsonschema.Schema) {
		in.Properties["db_driver"].Enum = []any{"postgres", "mysql", "sqlite"}
		in.Properties["rate"].Minimum = ptr(0.0)
	})
}

// ---- validate_config ----

// ValidateOut says whether a configuration is usable.
type ValidateOut struct {
	Valid     bool           `json:"valid"`
	Runners   []string       `json:"runners"`
	Duration  string         `json:"duration"`
	Targets   []string       `json:"targets"`
	Warnings  []plan.Warning `json:"warnings,omitempty"`
	Effective any            `json:"effective" jsonschema:"The configuration after defaults, with secrets redacted."`
}

func (o *ValidateOut) summary() string {
	return fmt.Sprintf("valid: %v over %s against %v", o.Runners, o.Duration, o.Targets)
}

func validateConfig() Operation {
	return define(Operation{
		Name:  "validate_config",
		Title: "Validate a configuration",
		Description: "Load and validate a configuration under this server's policy without contacting anything. Every problem is reported at once, each with a stable code, a JSON path, a line and a fix. " +
			"Call it after writing or changing a configuration and before plan_run or start_run.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "validate",
	}, func(ctx context.Context, s *Service, in *ConfigInput) (*ValidateOut, error) {
		req, err := s.request(*in)
		if err != nil {
			return nil, err
		}
		cfg, _, warnings, err := session.Load(ctx, req)
		if err != nil {
			return nil, err
		}
		out := &ValidateOut{Valid: true, Runners: cfg.Runners(), Duration: cfg.Run.Duration.String(),
			Targets: cfg.Targets(), Effective: cfg.Redacted()}
		for _, w := range warnings {
			out.Warnings = append(out.Warnings, plan.Warning{Code: w.Code, Message: w.Message, Fix: w.Fix})
		}
		return out, nil
	}, shapeConfig)
}

// shapeConfig says what the free-form config argument accepts.
func shapeConfig(in, _ *jsonschema.Schema) {
	if c := in.Properties["config"]; c != nil {
		c.Types = []string{"string", "object"}
	}
}

// ---- plan_run ----

func planRun() Operation {
	return define(Operation{
		Name:  "plan_run",
		Title: "Plan a run without running it",
		Description: "Describe exactly what a run would do - offered load per runner, targets, SLO budgets and the effective safety envelope - without contacting anything. " +
			"Call it before start_run to confirm the load and the targets are what the human agreed to.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "run --dry-run",
	}, func(ctx context.Context, s *Service, in *ConfigInput) (*plan.Plan, error) {
		req, err := s.request(*in)
		if err != nil {
			return nil, err
		}
		cfg, eff, warnings, err := session.Load(ctx, req)
		if err != nil {
			return nil, err
		}
		p := plan.Build(cfg, eff, warnings, req.SourcePath, in.Overrides)
		return &p, nil
	}, shapeConfig)
}

// ---- start_run ----

// StartIn starts a run.
type StartIn struct {
	ConfigInput
	Seed         *uint64 `json:"seed,omitempty" jsonschema:"Seed for every random choice; recorded in the result. Reuse a run's seed to reproduce it."`
	AllowInvalid bool    `json:"allow_invalid,omitempty" jsonschema:"Resolve an invalid run to exit 0 instead of 4. Almost never what you want: an invalid run measured the generator, not the target."`
}

// StartOut is a run that has started.
type StartOut struct {
	RunID  string `json:"run_id" jsonschema:"Pass to wait_for_run, get_run_status, get_run_digest and stop_run."`
	RunDir string `json:"run_dir"`
	Status string `json:"status"`
	PID    int    `json:"pid,omitempty"`
}

func (o *StartOut) summary() string {
	return fmt.Sprintf("started run %s; call wait_for_run with this run_id", o.RunID)
}

func startRun() Operation {
	return define(Operation{
		Name:  "start_run",
		Title: "Start a load test",
		Description: "Validate, preflight and start a run in the background, returning its run_id at once; load tests outlive tool calls. Configuration errors, policy refusals and preflight failures are returned synchronously, before any load. " +
			"Then call wait_for_run until it finishes and read get_run_digest. Start with a short smoke run (30s) and only run one test against a target at a time. It generates real load against the targets.",
		Annotations: Annotations{OpenWorld: true},
		CLI:         "run --detach",
	}, func(ctx context.Context, s *Service, in *StartIn) (*StartOut, error) {
		req, err := s.request(in.ConfigInput)
		if err != nil {
			return nil, err
		}
		req.Seed, req.AllowInvalid, req.Detached = in.Seed, in.AllowInvalid, true
		p, err := session.Prepare(ctx, req)
		if err != nil {
			return nil, err
		}
		if perr := p.Preflight(ctx); perr != nil {
			return nil, perr
		}
		pid, err := s.launch(ctx, p)
		if err != nil {
			return nil, err
		}
		return &StartOut{RunID: p.ID(), RunDir: p.Run.Dir, Status: runstore.StatusPending, PID: pid}, nil
	}, shapeConfig)
}

// ---- get_run_status ----

// RunRef names a run.
type RunRef struct {
	RunID string `json:"run_id" jsonschema:"A run id from start_run or list_runs; a unique prefix or suffix of one also works."`
}

func getRunStatus() Operation {
	return define(Operation{
		Name:  "get_run_status",
		Title: "Get a run's status",
		Description: "Return a run's state without waiting: pending, running, completed, interrupted, aborted, failed or lost, with progress, heartbeat and exit code. " +
			"Use wait_for_run instead when you want to block until it finishes.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "status",
	}, func(_ context.Context, s *Service, in *RunRef) (*runstore.State, error) {
		r, err := s.run(in.RunID)
		if err != nil {
			return nil, err
		}
		st, err := r.Status()
		if err != nil {
			return nil, err
		}
		return &st, nil
	}, nil)
}

// ---- wait_for_run ----

// WaitIn waits a bounded time.
type WaitIn struct {
	RunID    string `json:"run_id" jsonschema:"The run to wait for."`
	TimeoutS int    `json:"timeout_s,omitempty" jsonschema:"How long to wait in this call, 1 to 50 seconds (default 45). If the run is still going when it expires, call again."`
}

// WaitOut is where a run is after waiting.
type WaitOut struct {
	Finished bool           `json:"finished" jsonschema:"True when the run reached a final status. False means it is still going: call wait_for_run again."`
	State    runstore.State `json:"state"`
	ExitCode *int           `json:"exit_code,omitempty" jsonschema:"The run's exit code, once finished: 0 valid and within budget, 1 budget breached, 3 failed or cut short, 4 invalid."`
	Digest   *result.Digest `json:"digest,omitempty" jsonschema:"The run's digest, once finished - the same document get_run_digest returns."`
}

func (o *WaitOut) summary() string {
	if !o.Finished {
		return fmt.Sprintf("run %s is still %s; call wait_for_run again", o.State.RunID, o.State.Status)
	}
	if o.Digest != nil {
		return fmt.Sprintf("run %s %s: validity %s, verdict %s (%s confidence)", o.State.RunID, o.State.Status,
			o.Digest.Validity.State, o.Digest.Verdict.Bottleneck, o.Digest.Verdict.Confidence)
	}
	return fmt.Sprintf("run %s %s", o.State.RunID, o.State.Status)
}

func waitForRun() Operation {
	return define(Operation{
		Name:  "wait_for_run",
		Title: "Wait for a run to finish",
		Description: fmt.Sprintf("Wait up to timeout_s seconds (at most %d) for a run to finish. Returns finished=false with the current state if it is still going - call again - or the final state, exit code and digest when done. ", MaxWait) +
			"Read the digest's validity first: an invalid run measured the generator, not the target, and its verdict must not be reported.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "wait",
	}, func(ctx context.Context, s *Service, in *WaitIn) (*WaitOut, error) {
		r, err := s.run(in.RunID)
		if err != nil {
			return nil, err
		}
		secs := in.TimeoutS
		if secs <= 0 {
			secs = 45
		}
		wctx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
		st, err := r.Wait(wctx, 250*time.Millisecond)
		out := &WaitOut{State: st}
		if err != nil {
			var typed *errs.Error
			if errors.As(err, &typed) && typed.Code == errs.CodeWaitTimeout && ctx.Err() == nil {
				return out, nil // still running is an answer, not a failure
			}
			return nil, err
		}
		out.Finished, out.ExitCode = true, st.ExitCode
		if d, derr := digestOf(r, 0); derr == nil {
			out.Digest = d
		}
		return out, nil
	}, func(in, _ *jsonschema.Schema) {
		in.Properties["timeout_s"].Minimum = ptr(0.0)
		in.Properties["timeout_s"].Maximum = ptr(float64(MaxWait))
	})
}

// ---- stop_run ----

// StopOut says whether a stop was requested.
type StopOut struct {
	RunID   string `json:"run_id"`
	Stopped bool   `json:"stopped" jsonschema:"True when a stop was requested; false when the run had already ended."`
	Status  string `json:"status"`
}

func (o *StopOut) summary() string {
	if o.Stopped {
		return fmt.Sprintf("asked run %s to stop; wait_for_run returns its partial result", o.RunID)
	}
	return fmt.Sprintf("run %s had already ended: %s", o.RunID, o.Status)
}

func stopRun() Operation {
	return define(Operation{
		Name:  "stop_run",
		Title: "Stop a run gracefully",
		Description: "Ask a running test to stop. It stops offering load within half a second, drains in-flight work and writes a partial result marked interrupted, which wait_for_run then returns. " +
			"Use it when a run is no longer wanted or is hurting a target; stopping an ended run does nothing.",
		Annotations: Annotations{Idempotent: true},
		CLI:         "stop",
	}, func(_ context.Context, s *Service, in *RunRef) (*StopOut, error) {
		r, err := s.run(in.RunID)
		if err != nil {
			return nil, err
		}
		st, err := r.Status()
		if err != nil {
			return nil, err
		}
		if runstore.Terminal(st.Status) {
			return &StopOut{RunID: r.ID, Status: st.Status}, nil
		}
		if err := r.RequestStop(); err != nil {
			return nil, err
		}
		return &StopOut{RunID: r.ID, Stopped: true, Status: st.Status}, nil
	}, nil)
}

// ---- get_run_digest ----

// DigestIn asks for a digest.
type DigestIn struct {
	RunID       string `json:"run_id" jsonschema:"The run to summarise."`
	BudgetChars int    `json:"budget_chars,omitempty" jsonschema:"Largest digest to return, in characters (default 4000). When content is dropped to fit, truncated is true and more says what to fetch with get_run_section."`
}

func getRunDigest() Operation {
	return define(Operation{
		Name:  "get_run_digest",
		Title: "Get a run's digest",
		Description: "Return the compact, prioritised summary of a finished run: validity, SLO outcome, the verdict with evidence and confidence, the worst incidents, headline numbers per tier, machine-applicable recommendations and artifact paths - in that order. " +
			"Read it before anything else and stop when you have the answer. A recommendation with action rerun carries the exact overrides to pass to start_run. Correlation is not causation; say so when reporting.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "digest",
	}, func(_ context.Context, s *Service, in *DigestIn) (*result.Digest, error) {
		r, err := s.run(in.RunID)
		if err != nil {
			return nil, err
		}
		return digestOf(r, in.BudgetChars)
	}, func(in, _ *jsonschema.Schema) {
		in.Properties["budget_chars"].Minimum = ptr(0.0)
	})
}

// digestOf builds a run's digest from its result.
func digestOf(r *runstore.Run, budget int) (*result.Digest, error) {
	res, err := readResult(r)
	if err != nil {
		return nil, err
	}
	return result.BuildDigest(res, result.DigestOptions{BudgetChars: budget, Ref: r.ID}), nil
}

func readResult(r *runstore.Run) (*result.Result, error) {
	data, err := os.ReadFile(r.Path(runstore.ResultFile))
	if err != nil {
		status := "in an unknown state"
		if st, serr := r.Status(); serr == nil {
			status = string(st.Status)
		}
		return nil, errs.New(errs.CodeResultNotFound, "run %s has no result yet: it is %s", r.ID, status).
			WithHint("wait_for_run until it finishes; a failed or lost run never has one")
	}
	return result.Decode(data)
}

// ---- list_runs ----

// ListIn filters the run list.
type ListIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"At most this many runs, newest first (default 20)."`
}

// RunSummary is one line of the run list.
type RunSummary struct {
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Actor     string `json:"actor,omitempty"`
}

// ListOut is the run list.
type ListOut struct {
	Runs []RunSummary `json:"runs"`
}

func (o *ListOut) summary() string { return fmt.Sprintf("%d run(s)", len(o.Runs)) }

func listRuns() Operation {
	return define(Operation{
		Name:        "list_runs",
		Title:       "List runs",
		Description: "List runs newest first with status and exit code. Use it to find a run_id, or to see whether a run is already active before starting another - only one test should load a target at a time.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "list",
	}, func(_ context.Context, s *Service, in *ListIn) (*ListOut, error) {
		all, err := s.Store.List()
		if err != nil {
			return nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		out := &ListOut{Runs: []RunSummary{}}
		for i, st := range all {
			if i == limit {
				break
			}
			out.Runs = append(out.Runs, RunSummary{RunID: st.RunID, Status: st.Status, CreatedAt: st.CreatedAt, ExitCode: st.ExitCode, Actor: st.Actor})
		}
		return out, nil
	}, func(in, _ *jsonschema.Schema) {
		in.Properties["limit"].Minimum = ptr(0.0)
	})
}
