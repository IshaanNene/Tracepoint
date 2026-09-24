// Command tracepoint drives correlated HTTP, SQL and Redis load and reports which
// tier a slowdown came from.
//
// This file is wiring only: it assembles the environment the command tree needs -
// streams, environment lookup, terminal detection - and maps the result to a process
// exit code. All behaviour lives under internal/ and in the public API at the module
// root.
package main

import (
	"context"
	"os"

	"github.com/IshaanNene/Tracepoint/internal/cli"
)

func main() {
	os.Exit(cli.Execute(context.Background(), cli.Env{
		Stdin:    os.Stdin,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Args:     os.Args[1:],
		Lookup:   os.LookupEnv,
		IsTTY:    isTerminal(os.Stdout),
		ErrTTY:   isTerminal(os.Stderr),
		NoColour: os.Getenv("NO_COLOR") != "",
		CI:       os.Getenv("CI") != "",
	}))
}

// isTerminal reports whether f is a character device. Colour and the live view are
// used only when it is; everywhere else - a pipe, a log file, a CI job - the output
// stays plain, because a terminal escape in a log is noise.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
