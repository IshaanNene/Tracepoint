package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/compare"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	reports "github.com/IshaanNene/Tracepoint/internal/render/report"
)

func newCompareCmd(env Env, g *globals) *cobra.Command {
	var (
		format  string
		out     string
		gates   compare.Gates
		budgets []string
		sf      storeFlags
	)
	cmd := &cobra.Command{
		Use:   "compare <baseline> <current>",
		Short: "Compare two runs, with confidence intervals and regression gates",
		Long: strings.TrimSpace(`
Compare a run against a baseline - each a run id, a run directory or a result.json -
and exit 1 when a gate fails, for CI.

A p99 change is reported only when the two runs' 95% confidence intervals do not
overlap, so sampling noise is never called a regression. The gates:

  budget crossing       the current p99 is over its budget and the baseline's was not
  relative increase     p99 rose by more than --gate-relative-pct and
                        --gate-relative-ms, and significantly
  error-rate increase   the error rate rose by more than --gate-error-pp points

A rise large enough to cross the relative gate on too few samples to call real fails
as COMPARE_INSUFFICIENT_DATA: it can be neither confirmed nor cleared. Budgets come
from the runs' own SLOs unless --budget names them.

  table     for a terminal (the default)
  json      the comparison document; the same as --output json
  markdown  for a pull-request comment
  junit     one test case per gate
  html      an offline page with both runs' timelines overlaid`),
		Example: strings.TrimSpace(`
  tracepoint compare 20260924T100000Z-abc123 20260925T100000Z-def456
  tracepoint compare baseline/result.json runs/latest/result.json --format markdown >> "$GITHUB_STEP_SUMMARY"
  tracepoint compare base.json head.json --format junit --out compare.xml
  tracepoint compare base.json head.json --gate-relative-pct 5 --budget http=250
  tracepoint compare base.json head.json --output json`),
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if g.json() {
				format = reports.JSON
			}
			f, err := reports.NormaliseCompare(format)
			if err != nil {
				return err
			}
			if len(budgets) > 0 {
				gates.Budgets = map[string]float64{}
				for _, kv := range budgets {
					name, v, ok := strings.Cut(kv, "=")
					ms, perr := strconv.ParseFloat(strings.TrimSuffix(v, "ms"), 64)
					if !ok || perr != nil || ms <= 0 {
						return errs.New(errs.CodeOpsInvalidInput, "--budget %q is not runner=milliseconds", kv).
							WithHint("for example --budget http=250")
					}
					gates.Budgets[name] = ms
				}
			}
			b, _, err := loadResult(env, sf, args[0])
			if err != nil {
				return err
			}
			c, _, err := loadResult(env, sf, args[1])
			if err != nil {
				return err
			}
			rep, err := compare.Compare(b, c, compare.Options{Gates: gates})
			if err != nil {
				return err
			}
			page, err := reports.RenderCompare(rep, b, c, f, 0)
			if err != nil {
				return err
			}
			if out == "" && f == reports.HTML {
				out = reports.CompareFileName(f)
			}
			if out == "" || out == "-" {
				if _, err := env.Stdout.Write(page); err != nil {
					return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the comparison")
				}
			} else {
				if err := os.WriteFile(out, page, 0o600); err != nil {
					return errs.Wrap(errs.CodeIOWriteFailed, err, "writing %s", out)
				}
				if !g.json() {
					fmt.Fprintln(env.Stderr, "wrote", out)
				}
			}
			if rep.Regression {
				return &exitError{code: rep.ExitCode(), documented: true,
					err: errs.New(rep.ErrorCode(), "%s", rep.Summary)}
			}
			return nil
		},
	}
	sf.register(cmd)
	f := cmd.Flags()
	f.StringVar(&format, "format", reports.Table, "table, json, markdown, junit or html")
	f.StringVar(&out, "out", "", "write the comparison here instead of stdout (html defaults to compare.html)")
	f.Float64Var(&gates.RelativePct, "gate-relative-pct", compare.DefaultGates().RelativePct, "relative gate: a significant p99 rise above this percentage")
	f.Float64Var(&gates.RelativeMS, "gate-relative-ms", compare.DefaultGates().RelativeMS, "relative gate: and above this many milliseconds")
	f.Float64Var(&gates.ErrorPP, "gate-error-pp", compare.DefaultGates().ErrorPP, "error-rate gate, in percentage points")
	f.StringArrayVar(&budgets, "budget", nil, "p99 budget for the budget-crossing gate, runner=ms; repeatable")
	return cmd
}
