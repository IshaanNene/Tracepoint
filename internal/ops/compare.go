package ops

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/IshaanNene/Tracepoint/internal/compare"
	reports "github.com/IshaanNene/Tracepoint/internal/render/report"
)

// CompareIn names two finished runs and, optionally, the gates.
type CompareIn struct {
	BaselineRunID string             `json:"baseline_run_id" jsonschema:"The run to compare against: usually the last known-good run of the same configuration."`
	CurrentRunID  string             `json:"current_run_id" jsonschema:"The run being judged."`
	Format        string             `json:"format,omitempty" jsonschema:"json (default): the comparison document alone. markdown: also a summary sized for a pull-request comment, in content. junit: also JUnit XML, in content. html: also the offline comparison page, written to the current run's directory; only its path is returned."`
	RelativePct   float64            `json:"relative_pct,omitempty" jsonschema:"The relative gate: a p99 rise counts as a regression only above this percentage (and relative_ms), and only when significant. Default 10."`
	RelativeMS    float64            `json:"relative_ms,omitempty" jsonschema:"The relative gate's absolute floor in milliseconds. Default 5."`
	ErrorPP       float64            `json:"error_pp,omitempty" jsonschema:"The error-rate gate, in percentage points. Default 0.5."`
	BudgetsMS     map[string]float64 `json:"budgets_ms,omitempty" jsonschema:"p99 budgets in milliseconds by runner, for the budget-crossing gate. Default: the runs' own SLOs."`
}

// CompareOut is a comparison.
type CompareOut struct {
	Comparison *compare.Report `json:"comparison" jsonschema:"The comparison. Read summary, then gates; regression is true when any gate failed. The same document tracepoint compare --output json prints."`
	Format     string          `json:"format"`
	Content    string          `json:"content,omitempty" jsonschema:"The rendered markdown or junit, when asked for."`
	Path       string          `json:"path,omitempty" jsonschema:"Where the html page or rendered format was written, in the current run's directory."`
}

func (o *CompareOut) summary() string { return o.Comparison.Summary }

func compareRuns() Operation {
	return define(Operation{
		Name:  "compare_runs",
		Title: "Compare two runs",
		Description: "Compare a finished run against a baseline: p50, p95 and p99, error rate and throughput per runner and label, with distribution-free 95% confidence intervals for p99, " +
			"so a change is reported only when the intervals do not overlap - sampling noise is never called a regression. " +
			"Gates fail on a p99 crossing its budget, a significant p99 rise over relative_pct and relative_ms, or an error-rate rise over error_pp; regression is true when any failed. " +
			"Incidents are paired across the runs as new, fixed, worsened, improved or unchanged. Compare runs of the same configuration, both valid: the comparison warns when they are not.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
		CLI:         "compare",
	}, func(_ context.Context, s *Service, in *CompareIn) (*CompareOut, error) {
		format := in.Format
		if format == "" {
			format = reports.JSON
		}
		format, err := reports.NormaliseCompare(format)
		if err != nil {
			return nil, err
		}
		if format == reports.Table {
			format = reports.JSON // a terminal table helps nobody reading JSON
		}
		br, err := s.run(in.BaselineRunID)
		if err != nil {
			return nil, err
		}
		cr, err := s.run(in.CurrentRunID)
		if err != nil {
			return nil, err
		}
		b, err := readResult(br)
		if err != nil {
			return nil, err
		}
		c, err := readResult(cr)
		if err != nil {
			return nil, err
		}
		rep, err := compare.Compare(b, c, compare.Options{Gates: compare.Gates{
			RelativePct: in.RelativePct, RelativeMS: in.RelativeMS, ErrorPP: in.ErrorPP, Budgets: in.BudgetsMS,
		}})
		if err != nil {
			return nil, err
		}
		out := &CompareOut{Comparison: rep, Format: format}
		if format == reports.JSON {
			return out, nil
		}
		page, err := reports.RenderCompare(rep, b, c, format, 0)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("compare-%s.%s", br.ID, map[string]string{reports.Markdown: "md", reports.JUnit: "xml", reports.HTML: "html"}[format])
		if err := cr.WriteFile(name, page); err != nil {
			return nil, err
		}
		out.Path = cr.Path(name)
		if format != reports.HTML {
			out.Content = string(page)
		}
		return out, nil
	}, func(in, _ *jsonschema.Schema) {
		in.Properties["format"].Enum = []any{reports.JSON, reports.Markdown, reports.JUnit, reports.HTML}
		for _, k := range []string{"relative_pct", "relative_ms", "error_pp"} {
			in.Properties[k].Minimum = ptr(0.0)
		}
	})
}
