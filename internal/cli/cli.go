// Package cli builds TracePoint's command tree.
//
// It is an adapter and holds no behaviour of its own: each command parses flags, calls
// into the core, and turns the outcome into output and an exit code. Two contracts
// matter here and are enforced in one place, below:
//
//   - With --output json, stdout carries exactly one JSON document and nothing else.
//     Logs, progress and warnings go to stderr. An agent parsing that stream has no
//     fallback, so a stray Println would be a broken contract, not a cosmetic bug.
//   - Exit codes mean what docs/ERRORS.md says they mean, and their precedence when
//     several apply is 2 > 3 > 4 > 1.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strings"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Env is everything a command may touch outside itself. Injected so the whole tree is
// testable without touching the process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Args   []string
	// Lookup resolves environment variables for ${ENV} interpolation.
	Lookup func(string) (string, bool)
	// IsTTY reports whether stdout is a terminal. Colour and the live view are used
	// only when it is.
	IsTTY bool
	// NoColour forces plain output, from NO_COLOR or --no-color.
	NoColour bool
	// CI reports whether this is a CI environment, which forbids prompting.
	CI bool
	// Exit ends the process. Injected so that the one place which genuinely has to
	// exit immediately - a second interrupt - can be exercised by a test rather than
	// taking the test binary down with it. Defaults to os.Exit.
	Exit func(int)
	// RunRoot is where run directories are created when neither --run-root nor the
	// policy says. Injected so tests never write into the source tree.
	RunRoot string
	// Executable is the binary a detached run re-executes. Defaults to this one.
	Executable string
	// Actor names whoever is running commands, for the audit log. Defaults to
	// cli:<user>.
	Actor string
}

// actor names the caller for the audit log.
func (e Env) actor() string {
	if e.Actor != "" {
		return e.Actor
	}
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return "cli:" + name
}

func (e Env) exit(code int) {
	if e.Exit != nil {
		e.Exit(code)
		return
	}
	os.Exit(code)
}

// globals are the flags every command shares.
type globals struct {
	output         string
	logFormat      string
	logLevel       string
	noColour       bool
	nonInteractive bool
}

const (
	outputHuman = "human"
	outputJSON  = "json"
)

func (g *globals) json() bool { return g.output == outputJSON }

// Execute builds and runs the command tree, returning the process exit code.
func Execute(ctx context.Context, env Env) int {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Lookup == nil {
		env.Lookup = os.LookupEnv
	}

	g := &globals{output: outputHuman, logFormat: "text", logLevel: "info"}
	// Each command's RunE takes the context from cobra, which ExecuteContext below
	// populates from ctx; the analyser cannot see through that indirection.
	//nolint:contextcheck // the context reaches every command via cmd.Context()
	root := newRoot(env, g)
	root.SetArgs(env.Args)
	root.SetOut(env.Stderr) // help and usage are not the JSON document
	root.SetErr(env.Stderr)
	root.SetIn(env.Stdin)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return errs.ExitOK
	}
	return report(env, g, err)
}

// report writes a failure in the requested form and returns its exit code.
//
// In JSON mode the envelope is the single document on stdout, so a caller that parses
// stdout gets either a result or an error and never has to guess which.
func report(env Env, g *globals, err error) int {
	var exit *exitError
	if errors.As(err, &exit) {
		if exit.err == nil {
			return exit.code
		}
		err = exit.err
	}

	code := errs.ExitCodeOf(err)
	if exit != nil && exit.code != 0 {
		code = exit.code
	}

	// The outcome is already in what the command printed. Say it once more in a single
	// line for a human reading a terminal, and say nothing at all in JSON mode, where
	// a second document would corrupt the stream.
	if exit != nil && exit.documented {
		if !g.json() {
			fmt.Fprintf(env.Stderr, "\n%s\n", summarise(err))
		}
		return code
	}

	if g.json() {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(errs.EnvelopeOf(err)); encErr != nil {
			fmt.Fprintln(env.Stderr, "tracepoint: writing the error envelope:", encErr)
		}
		return code
	}

	writeHumanError(env.Stderr, err)
	return code
}

// writeHumanError prints a failure the way a person wants to read it: what went wrong,
// where, and what to do about it.
func writeHumanError(w io.Writer, err error) {
	var typed *errs.Error
	if !errors.As(err, &typed) {
		fmt.Fprintf(w, "tracepoint: %v\n", err)
		return
	}
	fmt.Fprintf(w, "tracepoint: %s\n", typed.Message)
	printDetail(w, typed, "  ")
	for _, c := range typed.Causes {
		fmt.Fprintf(w, "\n  %s\n", c.Message)
		printDetail(w, c, "    ")
	}
	fmt.Fprintf(w, "\n  %s\n", dimCode(typed.Code))
}

func printDetail(w io.Writer, e *errs.Error, indent string) {
	if e.Path != "" {
		loc := e.Path
		if e.Line > 0 {
			loc = fmt.Sprintf("%s (line %d", e.Path, e.Line)
			if e.Column > 0 {
				loc += fmt.Sprintf(", column %d", e.Column)
			}
			loc += ")"
		}
		fmt.Fprintf(w, "%sat %s\n", indent, loc)
	} else if e.Line > 0 {
		fmt.Fprintf(w, "%sat line %d\n", indent, e.Line)
	}
	if e.Hint != "" {
		fmt.Fprintf(w, "%s%s\n", indent, e.Hint)
	}
}

func dimCode(c errs.Code) string { return "error code: " + string(c) }

// summarise renders an already-reported outcome as one line.
func summarise(err error) string {
	var typed *errs.Error
	if errors.As(err, &typed) {
		return fmt.Sprintf("%s: %s", typed.Code, typed.Message)
	}
	return err.Error()
}

// exitError carries an exit code for outcomes that are not failures of the tool - an
// SLO breach, an invalid run - where there is a result to report and a non-zero code
// to return alongside it.
//
// documented says the outcome has already been described in the output the command
// wrote. That matters because of the §6.1 contract: with --output json, stdout must
// carry exactly one JSON document. A run that breaches its SLO has already put the
// result there, and the breach is a field inside it, so emitting an error envelope
// afterwards would put two documents on the stream and break every parser reading it.
type exitError struct {
	code       int
	err        error
	documented bool
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit %d", e.code)
}

func (e *exitError) Unwrap() error { return e.err }

func newRoot(env Env, g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "tracepoint",
		Short: "Correlated HTTP, SQL and Redis load testing that says which tier is slow",
		Long: strings.TrimSpace(`
TracePoint drives HTTP, SQL and Redis load concurrently on one monotonic clock, records
every layer into the same time buckets, and reports - with evidence and a confidence
level - whether a slowdown came from the application tier, the database or the cache.

Storage runners are probes as well as load: they hit the datastore directly, so their
latency is a direct measurement of that tier's health while the application is under
pressure.`),
		SilenceUsage:  true,
		SilenceErrors: true,
		Example: strings.TrimSpace(`
  # Check a configuration without running anything
  tracepoint validate -c tracepoint.yaml

  # Run it, and print a summary
  tracepoint run -c tracepoint.yaml

  # Run it from a script or an agent: one JSON document on stdout
  tracepoint run -c tracepoint.yaml --output json

  # Emit a contract so a tool can generate against it
  tracepoint schema config`),
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&g.output, "output", "o", outputHuman,
		"output format: human or json. With json, stdout carries exactly one JSON document and logs go to stderr")
	pf.StringVar(&g.logFormat, "log-format", "text", "log format on stderr: text or json")
	pf.StringVar(&g.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	pf.BoolVar(&g.noColour, "no-color", false, "disable colour even on a terminal")
	pf.BoolVar(&g.nonInteractive, "non-interactive", false, "never prompt; assume no terminal is attached")

	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		switch g.output {
		case outputHuman, outputJSON:
		default:
			return errs.New(errs.CodeOpsInvalidInput, "unknown output format %q", g.output).
				WithHint("use human or json")
		}
		return nil
	}

	root.AddCommand(
		newRunCmd(env, g),
		newValidateCmd(env, g),
		newDoctorCmd(env, g),
		newDigestCmd(env),
		newStatusCmd(env, g),
		newWaitCmd(env, g),
		newStopCmd(env, g),
		newListCmd(env, g),
		newGCCmd(env, g),
		newDetachedCmd(env),
		newSchemaCmd(env),
		newVersionCmd(env, g),
	)
	return root
}

// logger builds the stderr logger. It never writes to stdout, which in JSON mode
// belongs exclusively to the result document.
func (g *globals) logger(env Env) *slog.Logger {
	level := slog.LevelInfo
	switch g.logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if g.logFormat == "json" {
		return slog.New(slog.NewJSONHandler(env.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(env.Stderr, opts))
}

// colour reports whether ANSI should be used: only on a terminal, in human mode, with
// neither NO_COLOR nor --no-color set.
func (g *globals) colour(env Env) bool {
	return env.IsTTY && !env.NoColour && !g.noColour && !g.json()
}

// writeJSON puts exactly one document on stdout.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the output document")
	}
	return nil
}
