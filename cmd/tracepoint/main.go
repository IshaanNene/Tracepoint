// Command tracepoint drives correlated HTTP, SQL and Redis load and reports which
// tier a slowdown came from.
//
// This file is wiring only: it builds the dependency graph (clock, RNG, logger,
// streams, filesystem roots), hands it to the command tree and maps the resulting
// error to a process exit code. All behaviour lives in the packages under internal/
// and in the public API in the module root.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
)

// Exit codes are part of the CLI contract (docs/SPEC.md §6.1). Precedence when
// several apply: usage > runtime > invalid > breach.
const (
	exitOK       = 0 // success
	exitBreach   = 1 // SLO breach or regression
	exitUsage    = 2 // usage, config or policy refusal
	exitRuntime  = 3 // preflight or runtime failure, or aborted
	exitInvalid  = 4 // run invalid (unless --allow-invalid)
	exitWaitOpen = 5 // wait timed out while the run continues
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable core: no globals, explicit streams, an exit code out.
func run(args []string, stdout, stderr *os.File) int {
	// The cobra command tree is built in phase 1. Until then the skeleton answers
	// only `version`, which is enough to prove the build and ldflags wiring.
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(buildinfo.Get()); err != nil {
			fmt.Fprintln(stderr, "tracepoint: writing version:", err)
			return exitRuntime
		}
		return exitOK
	}
	fmt.Fprintln(stderr, "tracepoint: the command tree lands in phase 1; see docs/SPEC.md and docs/PROGRESS.md")
	return exitUsage
}
