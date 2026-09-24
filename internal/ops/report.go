package ops

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	reports "github.com/IshaanNene/Tracepoint/internal/render/report"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
)

// ReportIn names a run and a format.
type ReportIn struct {
	RunID  string `json:"run_id" jsonschema:"The finished run to render."`
	Format string `json:"format,omitempty" jsonschema:"markdown (default): a summary sized for a pull-request comment or CI job summary, returned inline. junit: JUnit XML whose failures match the exit codes, returned inline. html: the offline report for a human, written to the run directory; only its path is returned."`
}

// ReportOut is a rendered report.
type ReportOut struct {
	RunID   string `json:"run_id"`
	Format  string `json:"format"`
	Path    string `json:"path" jsonschema:"Where the report was written, inside the run directory."`
	Bytes   int    `json:"bytes"`
	Content string `json:"content,omitempty" jsonschema:"The report itself, for markdown and junit. Absent for html, which is for a person to open, not for a model to read: give the human the path."`
}

func (o *ReportOut) summary() string {
	return fmt.Sprintf("wrote the %s report for %s to %s (%d bytes)", o.Format, o.RunID, o.Path, o.Bytes)
}

func renderReport() Operation {
	return define(Operation{
		Name:  "render_report",
		Title: "Render a run's report",
		Description: "Render a finished run as Markdown, JUnit XML or the offline HTML report, from its result.json, and write it into the run directory. " +
			"Use markdown to post a summary on a pull request or in a CI job summary; use html to hand a human the full report (every run already writes report.html, so this only re-renders it). " +
			"For deciding what to do next, read get_run_digest instead: it is smaller and carries the recommendations.",
		Annotations: Annotations{Idempotent: true},
		CLI:         "report --format",
	}, func(_ context.Context, s *Service, in *ReportIn) (*ReportOut, error) {
		format := in.Format
		if format == "" {
			format = reports.Markdown
		}
		format, err := reports.Normalise(format)
		if err != nil {
			return nil, err
		}
		r, err := s.run(in.RunID)
		if err != nil {
			return nil, err
		}
		res, err := readResult(r)
		if err != nil {
			return nil, err
		}
		page, err := reports.Render(res, format, r.Path(runstore.ResultFile))
		if err != nil {
			return nil, err
		}
		name := reports.FileName(format)
		if err := r.WriteFile(name, page); err != nil {
			return nil, err
		}
		out := &ReportOut{RunID: r.ID, Format: format, Path: r.Path(name), Bytes: len(page)}
		if format != reports.HTML {
			out.Content = string(page)
		}
		return out, nil
	}, func(in, _ *jsonschema.Schema) {
		in.Properties["format"].Enum = []any{reports.Markdown, reports.JUnit, reports.HTML}
	})
}
