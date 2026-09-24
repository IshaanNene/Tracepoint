package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

func newDigestCmd(env Env) *cobra.Command {
	var budget int
	cmd := &cobra.Command{
		Use:   "digest <result.json|run-dir>",
		Short: "Print the compact, prioritised summary of a run that an agent reads first",
		Long: strings.TrimSpace(`
Print the digest of a finished run: validity, SLO outcome, the verdict with its evidence,
the worst incidents, headline numbers per tier, machine-applicable recommendations and
where the full data lives - in that order, which is the order of priority.

The digest is always one JSON document, whatever --output says, because it exists to be
read by a program. It is cut to --budget-chars by dropping the lowest-priority content
first; when anything was dropped, truncated is true and more says how to get it.

A recommendation whose action is rerun carries the exact --set overrides that apply it.`),
		Example: strings.TrimSpace(`
  tracepoint digest runs/20260924T100000Z-abc123
  tracepoint digest result.json --budget-chars 16000
  tracepoint digest result.json | jq '.recommendations[] | select(.action == "rerun")'`),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := resolveResultPath(args[0])
			if err != nil {
				return err
			}
			res, err := readResult(path)
			if err != nil {
				return err
			}
			if budget == 0 {
				budget = -1 // --budget-chars 0 means no limit
			}
			d := result.BuildDigest(res, result.DigestOptions{BudgetChars: budget, Ref: args[0]})
			raw, err := d.Encode()
			if err != nil {
				return err
			}
			if _, err := env.Stdout.Write(raw); err != nil {
				return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the digest")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&budget, "budget-chars", result.DefaultDigestBudget,
		"cut the digest to at most this many characters, dropping the lowest-priority content first; 0 for no limit")
	return cmd
}

// resolveResultPath accepts a result file or a run directory that holds one.
func resolveResultPath(arg string) (string, error) {
	info, err := os.Stat(arg)
	switch {
	case err == nil && info.IsDir():
		p := filepath.Join(arg, "result.json")
		if _, statErr := os.Stat(p); statErr != nil {
			return "", errs.New(errs.CodeResultNotFound, "%s holds no result.json", arg).
				WithHint("the run may still be going, or may have failed before writing its result")
		}
		return p, nil
	case err == nil:
		return arg, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", errs.New(errs.CodeResultNotFound, "no result file or run directory at %s", arg).
			WithHint("pass the path to a result.json or to a run directory; run ids are resolved once the run store lands")
	default:
		return "", errs.Wrap(errs.CodeIOReadFailed, err, "reading %s", arg)
	}
}

func readResult(path string) (*result.Result, error) {
	data, err := os.ReadFile(path) //nolint:gosec // reading the file the user named is the point
	if err != nil {
		return nil, errs.Wrap(errs.CodeIOReadFailed, err, "reading %s", path)
	}
	return result.Decode(data)
}
