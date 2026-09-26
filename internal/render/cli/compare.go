package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/compare"
	"github.com/IshaanNene/Tracepoint/internal/render"
)

// RenderCompare writes a comparison for a terminal.
func RenderCompare(w io.Writer, rep *compare.Report, opts Options) error {
	if opts.Width <= 0 {
		opts.Width = 80
	}
	p := painter{on: opts.Colour}
	b := &strings.Builder{}
	badge := p.paint(green, "NO REGRESSION")
	if rep.Regression {
		badge = p.paint(red, "REGRESSION")
	}
	fmt.Fprintf(b, "\n  %s  %s -> %s\n\n", badge, rep.Baseline.ID, rep.Current.ID)
	fmt.Fprintf(b, "  %s\n\n", wrap(rep.Summary, opts.Width-4, "  "))
	for _, wn := range rep.Warnings {
		fmt.Fprintf(b, "  %s %s\n", p.paint(yellow, "!"), wrap(wn.Message, opts.Width-6, "    "))
	}
	if len(rep.Warnings) > 0 {
		b.WriteString("\n")
	}

	rows := [][]string{{"RUNNER", "P50", "P95", "P99", "DELTA P99", "ERRORS", "CHANGE"}}
	for _, d := range append(append([]compare.Delta(nil), rep.Runners...), rep.Labels...) {
		name := d.Runner
		if d.Label != "" {
			name += "/" + d.Label
		}
		rows = append(rows, []string{name,
			render.MS(d.Baseline.P50MS) + " > " + render.MS(d.Current.P50MS),
			render.MS(d.Baseline.P95MS) + " > " + render.MS(d.Current.P95MS),
			render.MS(d.Baseline.P99MS) + " > " + render.MS(d.Current.P99MS),
			fmt.Sprintf("%+.1f%%", d.P99Pct),
			render.Percent(d.Baseline.ErrorRatio) + " > " + render.Percent(d.Current.ErrorRatio),
			d.Change})
	}
	writeTable(b, p, rows, "  ")
	b.WriteString("\n")
	for _, g := range rep.Gates {
		if !g.Pass {
			fmt.Fprintf(b, "  %s %s\n", p.paint(red, "x"), wrap(g.Message, opts.Width-6, "    "))
		}
	}
	for _, inc := range rep.Incidents {
		if inc.Status != compare.StatusUnchanged {
			fmt.Fprintf(b, "  incident on %s: %s\n", inc.Runner, inc.Status)
		}
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}
