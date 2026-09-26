package markdown

import (
	"fmt"
	"io"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/compare"
	"github.com/IshaanNene/Tracepoint/internal/render"
)

// RenderCompare writes a comparison sized for a pull-request comment.
func RenderCompare(w io.Writer, rep *compare.Report) error {
	b := &strings.Builder{}
	verdict := "no regression"
	if rep.Regression {
		verdict = "**regression**"
	}
	fmt.Fprintf(b, "### TracePoint comparison: %s\n\n", verdict)
	fmt.Fprintf(b, "`%s` → `%s`\n\n%s\n\n", code(rep.Baseline.ID), code(rep.Current.ID), esc(rep.Summary))
	for _, wn := range rep.Warnings {
		fmt.Fprintf(b, "- **%s** `%s` %s\n", esc(wn.Severity), code(wn.Code), esc(wn.Message))
	}
	if len(rep.Warnings) > 0 {
		b.WriteString("\n")
	}

	b.WriteString("| Runner | p50 | p95 | p99 | Δ p99 | Errors | Throughput | Change |\n| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	for _, d := range rep.Runners {
		change := esc(d.Change)
		if d.Change == compare.ChangeSlower {
			change = "**slower**"
		}
		fmt.Fprintf(b, "| %s | %s → %s | %s → %s | %s → %s | %s | %s → %s | %s → %s/s | %s |\n", esc(d.Runner),
			render.MS(d.Baseline.P50MS), render.MS(d.Current.P50MS), render.MS(d.Baseline.P95MS), render.MS(d.Current.P95MS),
			render.MS(d.Baseline.P99MS), render.MS(d.Current.P99MS), signedPct(d.P99Pct),
			render.Percent(d.Baseline.ErrorRatio), render.Percent(d.Current.ErrorRatio),
			render.Number(d.Baseline.AchievedRPS), render.Number(d.Current.AchievedRPS), change)
	}
	b.WriteString("\n")

	var failed []compare.Gate
	for _, g := range rep.Gates {
		if !g.Pass {
			failed = append(failed, g)
		}
	}
	if len(failed) > 0 {
		b.WriteString("**Failed gates**\n\n")
		for _, g := range failed {
			fmt.Fprintf(b, "- `%s` %s\n", code(g.Name), esc(g.Message))
		}
		b.WriteString("\n")
	}
	if len(rep.Incidents) > 0 {
		b.WriteString("<details><summary>Incidents</summary>\n\n| Runner | Status | Baseline | Current |\n| --- | --- | --- | --- |\n")
		for _, inc := range rep.Incidents {
			fmt.Fprintf(b, "| %s | %s | %s | %s |\n", esc(inc.Runner), esc(inc.Status), incident(inc.Baseline), incident(inc.Current))
		}
		b.WriteString("\n</details>\n\n")
	}
	b.WriteString("> A p99 change counts only when the two runs' 95% confidence intervals do not overlap.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func incident(r *compare.IncidentRef) string {
	if r == nil {
		return "—"
	}
	return fmt.Sprintf("%s %s–%s, peak %s", esc(r.ID), render.Seconds(r.StartS), render.Seconds(r.EndS), render.MS(r.PeakP99MS))
}

func signedPct(v float64) string { return fmt.Sprintf("%+.1f%%", v) }
