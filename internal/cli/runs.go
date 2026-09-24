package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/session"
)

// storeFlags are the flags every run-store command shares.
type storeFlags struct {
	runRoot    string
	policyPath string
}

func (s *storeFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&s.runRoot, "run-root", "", "directory the runs live under; defaults to the policy's run_root, then runs/")
	cmd.Flags().StringVar(&s.policyPath, "policy", "", "safety policy file, for its run_root")
}

func (s *storeFlags) open(env Env) (*runstore.Store, error) {
	granted, err := policy.Load(s.policyPath, env.Lookup)
	if err != nil {
		return nil, err
	}
	return openStore(env, granted, s.runRoot)
}

func newStatusCmd(env Env, g *globals) *cobra.Command {
	var sf storeFlags
	cmd := &cobra.Command{
		Use:   "status <run>",
		Short: "Show what a run is doing, or how it ended",
		Long: strings.TrimSpace(`
Print a run's state.json: its status (pending, running, completed, interrupted,
aborted, failed or lost), progress, heartbeat, pid, exit code and artifact paths. A run
whose heartbeat went stale and whose process is gone is reported lost. The run may be
named by its id, a unique prefix or suffix of it, or its directory.`),
		Example: strings.TrimSpace(`
  tracepoint status 20260924T100000Z-abc123
  tracepoint status abc123 --output json`),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := sf.open(env)
			if err != nil {
				return err
			}
			r, err := store.Get(args[0])
			if err != nil {
				return err
			}
			st, err := r.Status()
			if err != nil {
				return err
			}
			return writeState(env, g, st)
		},
	}
	sf.register(cmd)
	return cmd
}

func newWaitCmd(env Env, g *globals) *cobra.Command {
	var (
		sf      storeFlags
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "wait <run>",
		Short: "Wait for a run to finish and exit with its exit code",
		Long: strings.TrimSpace(`
Wait for a run to reach a final status, print its state and exit with the run's own
exit code. With --timeout, give up after that long and exit 5: the run is still going,
and waiting again picks up where this left off. A lost run exits 3.`),
		Example: strings.TrimSpace(`
  tracepoint wait 20260924T100000Z-abc123
  tracepoint wait abc123 --timeout 50s --output json`),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := sf.open(env)
			if err != nil {
				return err
			}
			r, err := store.Get(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			st, waitErr := r.Wait(ctx, 250*time.Millisecond)
			if werr := writeState(env, g, st); werr != nil {
				return werr
			}
			if waitErr != nil {
				var typed *errs.Error
				if errors.As(waitErr, &typed) && typed.Code == errs.CodeWaitTimeout {
					return &exitError{code: errs.ExitWaitOpen, documented: true, err: waitErr}
				}
				return waitErr
			}
			return exitForState(st)
		},
	}
	sf.register(cmd)
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long and exit 5; 0 waits for as long as the run takes")
	return cmd
}

// exitForState turns a finished run's state into this command's exit code.
func exitForState(st runstore.State) error {
	if st.Status == runstore.StatusLost {
		return &exitError{code: errs.ExitRuntime, documented: true,
			err: errs.New(errs.CodeRunLost, "run %s was lost", st.RunID)}
	}
	if st.ExitCode != nil && *st.ExitCode != 0 {
		return &exitError{code: *st.ExitCode, documented: true,
			err: fmt.Errorf("run %s exited %d", st.RunID, *st.ExitCode)}
	}
	return nil
}

func newStopCmd(env Env, g *globals) *cobra.Command {
	var (
		sf    storeFlags
		force bool
	)
	cmd := &cobra.Command{
		Use:   "stop <run>",
		Short: "Stop a running test gracefully, or at once with --force",
		Long: strings.TrimSpace(`
Ask a run to stop. It notices within half a second, stops offering load, lets in-flight
work drain for its grace period and writes a partial result marked interrupted. The
request is a file in the run directory, so it works the same everywhere and from any
process. --force kills the run's process instead, and no result is written.`),
		Example: strings.TrimSpace(`
  tracepoint stop 20260924T100000Z-abc123
  tracepoint stop abc123 --force`),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := sf.open(env)
			if err != nil {
				return err
			}
			r, err := store.Get(args[0])
			if err != nil {
				return err
			}
			st, err := r.Status()
			if err != nil {
				return err
			}
			if runstore.Terminal(st.Status) {
				return writeJSONOrLine(env, g, map[string]any{"run_id": r.ID, "status": st.Status, "stopped": false},
					fmt.Sprintf("%s has already ended: %s", r.ID, st.Status))
			}
			if err := r.RequestStop(); err != nil {
				return err
			}
			if force {
				if err := kill(st.PID); err != nil {
					return err
				}
				code := errs.ExitRuntime
				if err := r.UpdateState(func(s *runstore.State) {
					s.Status, s.ExitCode = runstore.StatusFailed, &code
					s.Error = errs.EnvelopeOf(errs.New(errs.CodeRunFailed, "killed by stop --force"))
				}); err != nil {
					return err
				}
			}
			return writeJSONOrLine(env, g, map[string]any{"run_id": r.ID, "stopped": true, "forced": force},
				fmt.Sprintf("asked %s to stop", r.ID))
		},
	}
	sf.register(cmd)
	cmd.Flags().BoolVar(&force, "force", false, "kill the run's process instead of asking it to stop; no result is written")
	return cmd
}

func kill(pid int) error {
	if pid <= 0 || pid == os.Getpid() {
		return errs.New(errs.CodeOpsInvalidInput, "the run has no process that can be killed")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "finding process %d", pid)
	}
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errs.Wrap(errs.CodeInternal, err, "killing process %d", pid)
	}
	return nil
}

func newListCmd(env Env, g *globals) *cobra.Command {
	var (
		sf    storeFlags
		limit int
	)
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List runs, newest first",
		Example: "  tracepoint list\n  tracepoint list --limit 5 --output json",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			store, err := sf.open(env)
			if err != nil {
				return err
			}
			all, err := store.List()
			if err != nil {
				return err
			}
			if limit > 0 && len(all) > limit {
				all = all[:limit]
			}
			if g.json() {
				if all == nil {
					all = []runstore.State{}
				}
				return writeJSON(env.Stdout, map[string]any{"run_root": store.Root(), "runs": all})
			}
			if len(all) == 0 {
				fmt.Fprintf(env.Stdout, "no runs under %s\n", store.Root())
				return nil
			}
			for _, st := range all {
				code := "-"
				if st.ExitCode != nil {
					code = fmt.Sprint(*st.ExitCode)
				}
				fmt.Fprintf(env.Stdout, "%-26s  %-11s  exit %-2s  %s\n", st.RunID, st.Status, code, st.CreatedAt)
			}
			return nil
		},
	}
	sf.register(cmd)
	cmd.Flags().IntVar(&limit, "limit", 0, "show at most this many; 0 shows all")
	return cmd
}

func newGCCmd(env Env, g *globals) *cobra.Command {
	var (
		sf   storeFlags
		keep int
	)
	cmd := &cobra.Command{
		Use:     "gc",
		Short:   "Delete old finished runs, keeping the newest",
		Long:    "Delete finished runs beyond the newest --keep. Pending and running runs are never deleted.",
		Example: "  tracepoint gc --keep 20",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			store, err := sf.open(env)
			if err != nil {
				return err
			}
			removed, err := store.GC(keep)
			if err != nil {
				return err
			}
			if removed == nil {
				removed = []string{}
			}
			return writeJSONOrLine(env, g, map[string]any{"removed": removed, "kept": keep},
				fmt.Sprintf("removed %d run(s)", len(removed)))
		},
	}
	sf.register(cmd)
	cmd.Flags().IntVar(&keep, "keep", 20, "how many finished runs to keep")
	return cmd
}

// newDetachedCmd is the hidden entry point of a detached run's background process.
func newDetachedCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:    session.DetachedCommand,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A signal is a graceful stop, as in the foreground.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if code := session.RunDetached(ctx, env.Stdin, env.Lookup); code != 0 {
				return &exitError{code: code, documented: true}
			}
			return nil
		},
	}
}

func writeState(env Env, g *globals, st runstore.State) error {
	if g.json() {
		return writeJSON(env.Stdout, st)
	}
	fmt.Fprintf(env.Stdout, "%s  %s", st.RunID, st.Status)
	if st.Phase != "" && !runstore.Terminal(st.Status) {
		fmt.Fprintf(env.Stdout, " (%s)", st.Phase)
	}
	if st.Progress != nil && !runstore.Terminal(st.Status) {
		fmt.Fprintf(env.Stdout, "  %.0f%%", st.Progress.Ratio*100)
	}
	if st.ExitCode != nil {
		fmt.Fprintf(env.Stdout, "  exit %d", *st.ExitCode)
	}
	fmt.Fprintf(env.Stdout, "\n  run dir  %s\n", st.RunDir)
	return nil
}

func writeJSONOrLine(env Env, g *globals, doc any, line string) error {
	if g.json() {
		return writeJSON(env.Stdout, doc)
	}
	fmt.Fprintln(env.Stdout, line)
	return nil
}
