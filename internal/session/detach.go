package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
)

// DetachedCommand is the hidden subcommand a detached run's child executes.
const DetachedCommand = "__run-detached"

// handoff is what a parent passes to its detached child, over a pipe rather than a
// file: the configuration exactly as written may hold secrets supplied inline, and a
// pipe never touches the disk.
type handoff struct {
	Source       []byte             `json:"source"`
	SourcePath   string             `json:"source_path"`
	Overrides    []string           `json:"overrides,omitempty"`
	Granted      policy.Policy      `json:"granted"`
	Seed         uint64             `json:"seed"`
	Thresholds   map[string]float64 `json:"thresholds,omitempty"`
	AllowInvalid bool               `json:"allow_invalid,omitempty"`
	Actor        string             `json:"actor,omitempty"`
	RunID        string             `json:"run_id"`
	RunDir       string             `json:"run_dir"`
	ResultPath   string             `json:"result_path,omitempty"`
	ReportPath   string             `json:"report_path,omitempty"`
}

// Detach starts the run in a background process and returns once it has started.
// Call it after Preflight has passed, so everything that can be reported
// synchronously already has been (§6.4).
func (p *Prepared) Detach(ctx context.Context, executable string) (pid int, err error) {
	if !p.Policy.AllowDetach {
		return 0, errs.New(errs.CodePolicyDetachNotAllowed, "the policy does not allow background runs").
			WithHint("run in the foreground, or a human can set allow_detach: true")
	}
	if executable == "" {
		if executable, err = os.Executable(); err != nil {
			return 0, errs.Wrap(errs.CodeInternal, err, "finding this executable to start the background run")
		}
	}
	h := handoff{
		Source: p.req.Source, SourcePath: p.req.SourcePath, Overrides: p.req.Overrides,
		Granted: p.req.Granted, Seed: p.seed, Thresholds: p.req.Thresholds,
		AllowInvalid: p.req.AllowInvalid, Actor: p.req.Actor, RunID: p.Run.ID, RunDir: p.Run.Dir,
	}
	// Copies requested with --result-path and --report-path go where the caller meant,
	// whatever directory the child ends up in.
	if h.ResultPath, err = absolute(p.req.ResultPath); err != nil {
		return 0, err
	}
	if h.ReportPath, err = absolute(p.req.ReportPath); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(h)
	if err != nil {
		return 0, errs.Wrap(errs.CodeInternal, err, "encoding the hand-off to the background run")
	}

	logFile, err := os.OpenFile(p.Run.Path(runstore.LogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, errs.Wrap(errs.CodeIOWriteFailed, err, "opening the run log")
	}
	defer func() { _ = logFile.Close() }()

	// The child must outlive this process and this context: it is a new session, not
	// a child that dies with its parent's terminal.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), executable, DetachedCommand) //nolint:gosec // our own binary
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = detachAttrs()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, errs.Wrap(errs.CodeInternal, err, "connecting to the background run")
	}
	if err := cmd.Start(); err != nil {
		return 0, errs.Wrap(errs.CodeInternal, err, "starting the background run")
	}
	_, werr := stdin.Write(payload)
	cerr := stdin.Close()
	if werr != nil || cerr != nil {
		killErr := cmd.Process.Kill()
		return 0, errs.Wrap(errs.CodeInternal, errors.Join(werr, cerr, killErr), "handing the run to the background process")
	}
	pid = cmd.Process.Pid
	if err := p.Run.UpdateState(func(st *runstore.State) { st.PID, st.Detached = pid, true }); err != nil {
		return pid, err
	}
	// Release, not Wait: the parent is about to exit and the child is on its own.
	if err := cmd.Process.Release(); err != nil {
		return pid, errs.Wrap(errs.CodeInternal, err, "releasing the background run")
	}
	return pid, nil
}

// RunDetached is the background child: it reads the hand-off and executes the run in
// the directory its parent prepared. Its exit code is the run's.
func RunDetached(ctx context.Context, in io.Reader, lookup func(string) (string, bool)) int {
	var h handoff
	if err := json.NewDecoder(in).Decode(&h); err != nil {
		fmt.Fprintln(os.Stderr, "tracepoint: reading the hand-off from the parent:", err)
		return errs.ExitRuntime
	}
	store, err := runstore.Open(filepath.Dir(h.RunDir), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tracepoint:", err)
		return errs.ExitRuntime
	}
	seed := h.Seed
	req := Request{
		Source: h.Source, SourcePath: h.SourcePath, Overrides: h.Overrides, Granted: h.Granted,
		Lookup: lookup, Seed: &seed, Thresholds: h.Thresholds, AllowInvalid: h.AllowInvalid,
		Actor: h.Actor, Store: store, RunID: h.RunID, OutDir: h.RunDir, Detached: true,
		ResultPath: h.ResultPath, ReportPath: h.ReportPath,
		// run.log is written by the session itself; this process's stderr is also
		// run.log, so logging here as well would write every line twice.
		Logger: slog.New(slog.DiscardHandler),
	}
	p, err := Prepare(ctx, req)
	if err != nil {
		failRun(store, h, err)
		return errs.ExitCodeOf(err)
	}
	out, err := p.Execute(ctx)
	if err != nil {
		return errs.ExitCodeOf(err)
	}
	return out.ExitCode
}

// failRun records a child that could not even prepare - its configuration no longer
// loads, say - so the run is reported failed rather than left pending until it is
// declared lost.
func failRun(store *runstore.Store, h handoff, err error) {
	r, gerr := store.Get(h.RunDir)
	if gerr != nil {
		return
	}
	code := errs.ExitCodeOf(err)
	if uerr := r.UpdateState(func(st *runstore.State) {
		st.Status, st.ExitCode, st.Error = runstore.StatusFailed, &code, errs.EnvelopeOf(err)
	}); uerr != nil {
		fmt.Fprintln(os.Stderr, "tracepoint: recording the failure:", uerr)
	}
}

func absolute(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errs.Wrap(errs.CodeIOWriteFailed, err, "resolving %s", path)
	}
	return abs, nil
}
