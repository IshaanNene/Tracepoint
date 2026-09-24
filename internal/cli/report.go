package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	reports "github.com/IshaanNene/Tracepoint/internal/render/report"
)

// ReportOut is what `report --output json` prints: where the report went.
type ReportOut struct {
	RunID  string `json:"run_id"`
	Format string `json:"format"`
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
}

func newReportCmd(env Env, g *globals) *cobra.Command {
	var (
		format string
		out    string
		sf     storeFlags
	)
	cmd := &cobra.Command{
		Use:   "report <run|result.json|run-dir>",
		Short: "Render a finished run as HTML, Markdown or JUnit",
		Long: strings.TrimSpace(`
Render a run from its result.json - the same document the run wrote, so the report is
exactly what the run would have produced, and can be made again at any time.

  html      the single-file offline report (every run already writes report.html)
  markdown  a summary sized for a pull-request comment or CI job summary
  junit     JUnit XML whose failures match the exit codes: invalid run, SLO breach

HTML goes beside the result unless --out says otherwise. Markdown and JUnit go to
stdout, or to --out; with --output json every format is written to a file (beside
the result by default) and stdout carries only {run_id, format, path, bytes}.`),
		Example: strings.TrimSpace(`
  tracepoint report 20260924T100000Z-abc123
  tracepoint report runs/20260924T100000Z-abc123 --out /tmp/report.html
  tracepoint report result.json --format markdown >> "$GITHUB_STEP_SUMMARY"
  tracepoint report result.json --format junit --out junit.xml`),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			f, err := reports.Normalise(format)
			if err != nil {
				return err
			}
			res, path, err := loadResult(env, sf, args[0])
			if err != nil {
				return err
			}
			if out == "-" && g.json() {
				return errs.New(errs.CodeOpsInvalidInput, "--out - and --output json both want stdout").
					WithHint("name a file with --out, or drop --output json")
			}
			page, err := reports.Render(res, f, path)
			if err != nil {
				return err
			}
			if out == "" {
				out = filepath.Join(filepath.Dir(path), reports.FileName(f))
				if f != reports.HTML && !g.json() {
					out = "-"
				}
			}
			if out == "-" {
				if _, err := env.Stdout.Write(page); err != nil {
					return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the report")
				}
				return nil
			}
			if err := os.WriteFile(out, page, 0o600); err != nil {
				return errs.Wrap(errs.CodeIOWriteFailed, err, "writing %s", out)
			}
			return writeJSONOrLine(env, g, ReportOut{RunID: res.Run.ID, Format: f, Path: out, Bytes: len(page)},
				fmt.Sprintf("wrote %s", out))
		},
	}
	sf.register(cmd)
	cmd.Flags().StringVar(&format, "format", reports.HTML, "html, markdown or junit")
	cmd.Flags().StringVar(&out, "out", "", "write the report here; - for stdout")
	return cmd
}
