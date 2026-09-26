// Package cli renders a result as aligned terminal tables.
//
// Output is a pure function of result.json, so `tracepoint report` reproduces what the
// run printed. Colour is used only when the destination is a terminal and NO_COLOR is
// unset; the column layout is computed from the data either way, so piping the output
// somewhere does not change its shape.
package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/render"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Options control rendering.
type Options struct {
	// Colour turns on ANSI. Callers set it from isatty and NO_COLOR; the renderer
	// never inspects the environment itself, so its output stays reproducible.
	Colour bool
	// Verbose adds the per-label table.
	Verbose bool
	// Width is the terminal width used to wrap prose. Zero means 80.
	Width int
}

// ANSI codes, applied only when Options.Colour is set.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	cyan   = "\x1b[36m"
)

// labelWidth keeps the RUN / SLO / VERDICT labels in one column.
//
// Padding is always applied to the visible text and colour added afterwards. Using a
// printf width on an already-painted string counts the escape bytes towards it, which
// silently collapses the column the moment colour is enabled - the kind of bug that
// only shows up on the terminal a person is actually looking at.
const labelWidth = 9

type painter struct{ on bool }

func (p painter) paint(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return code + s + reset
}

// Render writes the human summary of a run.
func Render(w io.Writer, r *result.Result, opts Options) error {
	if opts.Width <= 0 {
		opts.Width = 80
	}
	p := painter{on: opts.Colour}
	b := &strings.Builder{}

	writeHeader(b, p, r)
	writeValidity(b, p, r, opts)
	writeSLO(b, p, r)
	writeRunners(b, p, r)
	if opts.Verbose {
		writeLabels(b, p, r)
	}
	writeIncidents(b, p, r)
	writeVerdict(b, p, r, opts)
	writeStrain(b, p, r, opts)
	writeCapacity(b, p, r, opts)
	writeArtifacts(b, p, r)

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing the summary: %w", err)
	}
	return nil
}

func writeHeader(b *strings.Builder, p painter, r *result.Result) {
	name := r.Run.Name
	if name == "" {
		name = r.Run.ID
	}
	fmt.Fprintf(b, "\n%s  %s\n", p.paint(bold, "TracePoint"), p.paint(dim, name))

	elapsed := r.Run.ElapsedMS
	if elapsed == 0 {
		elapsed = r.Run.DurationMS
	}
	detail := fmt.Sprintf("%s over %s · %d bucket(s) of %s · seed %d",
		r.Run.Status, render.MS(elapsed), r.Run.BucketCount, render.MS(r.Run.BucketMS), r.Run.Seed)
	if r.Run.InterruptedReason != "" {
		detail += " · " + r.Run.InterruptedReason
	}
	fmt.Fprintf(b, "  %s\n\n", p.paint(dim, detail))
}

func writeValidity(b *strings.Builder, p painter, r *result.Result, opts Options) {
	v := r.Analysis.Validity
	var badge string
	switch v.State {
	case result.ValidityValid:
		badge = p.paint(green, "VALID")
	case result.ValidityDegraded:
		badge = p.paint(yellow, "DEGRADED")
	default:
		badge = p.paint(red, "INVALID")
	}
	fmt.Fprintf(b, "  %s %s\n", p.paint(bold, pad("RUN", labelWidth)), badge)

	for _, f := range v.Findings {
		marker := p.paint(yellow, "!")
		if f.Severity == result.SeverityError {
			marker = p.paint(red, "x")
		}
		fmt.Fprintf(b, "    %s %s\n", marker, wrap(f.Message, opts.Width-6, "      "))
		if f.Fix != "" {
			fmt.Fprintf(b, "      %s\n", p.paint(dim, wrap("fix: "+f.Fix, opts.Width-6, "      ")))
		}
	}
	if len(v.Findings) > 0 {
		b.WriteString("\n")
	}
}

func writeSLO(b *strings.Builder, p painter, r *result.Result) {
	s := r.Analysis.SLO
	if len(s.Checks) == 0 {
		fmt.Fprintf(b, "  %s %s\n\n", p.paint(bold, pad("SLO", labelWidth)), p.paint(dim, "no budgets configured"))
		return
	}
	badge := p.paint(green, "PASS")
	if !s.Pass {
		badge = p.paint(red, "FAIL")
	}
	fmt.Fprintf(b, "  %s %s\n", p.paint(bold, pad("SLO", labelWidth)), badge)
	for _, c := range s.Checks {
		mark, colour := "ok", green
		if !c.Pass {
			mark, colour = "over", red
		}
		fmt.Fprintf(b, "    %s %s %s  %s %s\n",
			p.paint(colour, pad(mark, 4)),
			pad(c.Runner, 6), pad(c.Metric, 10),
			pad(formatBudget(c.Metric, c.Actual), 10),
			p.paint(dim, "budget "+formatBudget(c.Metric, c.Budget)))
	}
	b.WriteString("\n")
}

func formatBudget(metric string, v float64) string {
	if metric == "error_rate" {
		return fmt.Sprintf("%.2f%%", v*100)
	}
	return render.MS(v)
}

func writeRunners(b *strings.Builder, p painter, r *result.Result) {
	rows := [][]string{{"RUNNER", "KIND", "N", "RPS", "ERR", "p50", "p95", "p99", "max"}}
	for i := range r.Runners {
		rn := &r.Runners[i]
		rows = append(rows, []string{
			rn.Name, rn.Kind,
			strconv.FormatInt(rn.Summary.N, 10),
			fmt.Sprintf("%.1f", rn.Summary.AchievedRPS),
			fmt.Sprintf("%.2f%%", rn.Summary.ErrorRatio*100),
			render.MS(rn.Summary.Response.P50),
			render.MS(rn.Summary.Response.P95),
			render.MS(rn.Summary.Response.P99),
			render.MS(rn.Summary.Response.Max),
		})
	}
	writeTable(b, p, rows, "  ")

	// Client-side waiting is broken out whenever it is a material share of the
	// reported latency, because at that point the number describes us, not the target.
	for i := range r.Runners {
		rn := &r.Runners[i]
		if rn.Summary.Response.P99 <= 0 {
			continue
		}
		// A share of a tiny number is noise: a millisecond of scheduling jitter is most
		// of a 2ms p99 and says nothing about anything.
		if share := rn.Summary.ClientWait.P99 / rn.Summary.Response.P99; share > 0.2 && rn.Summary.ClientWait.P99 >= 5 {
			// Quoting the service figure alongside is the point: it is the number that
			// describes the target, and the one tier attribution uses. Without it a
			// reader sees a large p99 and blames the wrong thing.
			fmt.Fprintf(b, "  %s\n", p.paint(yellow, fmt.Sprintf(
				"%.0f%% of %s p99 (%s) was waiting inside the generator; %s itself took %s",
				share*100, rn.Name, render.MS(rn.Summary.Response.P99), rn.Name, render.MS(rn.Summary.Service.P99))))
		}
	}

	for i := range r.Runners {
		rn := &r.Runners[i]
		if rn.Executor == nil || rn.Executor.Dropped == 0 {
			continue
		}
		fmt.Fprintf(b, "  %s\n", p.paint(yellow, fmt.Sprintf(
			"%s dropped %d of %d arrivals (%.1f%%) because every worker was busy",
			rn.Name, rn.Executor.Dropped, rn.Executor.Offered, rn.Executor.DroppedRatio*100)))
	}

	var errored bool
	for i := range r.Runners {
		if len(r.Runners[i].Summary.Errors) > 0 {
			errored = true
		}
	}
	if errored {
		b.WriteString("\n")
		rows := [][]string{{"RUNNER", "ERRORS"}}
		for i := range r.Runners {
			rn := &r.Runners[i]
			if len(rn.Summary.Errors) == 0 {
				continue
			}
			rows = append(rows, []string{rn.Name, formatErrors(rn.Summary.Errors)})
		}
		writeTable(b, p, rows, "  ")
	}
	b.WriteString("\n")
}

func writeLabels(b *strings.Builder, p painter, r *result.Result) {
	for i := range r.Runners {
		rn := &r.Runners[i]
		labels := rn.SortedLabels()
		if len(labels) == 0 {
			continue
		}
		fmt.Fprintf(b, "  %s\n", p.paint(bold, rn.Name+" by label"))
		rows := [][]string{{"LABEL", "N", "RPS", "ERR", "p50", "p95", "p99"}}
		for _, name := range labels {
			s := rn.LabelSummaries[name]
			rows = append(rows, []string{
				name, strconv.FormatInt(s.N, 10),
				fmt.Sprintf("%.1f", s.AchievedRPS),
				fmt.Sprintf("%.2f%%", s.ErrorRatio*100),
				render.MS(s.Response.P50), render.MS(s.Response.P95), render.MS(s.Response.P99),
			})
		}
		writeTable(b, p, rows, "  ")
		b.WriteString("\n")
	}
}

// maxIncidentRows bounds the incident table; the full list is in result.json.
const maxIncidentRows = 10

func writeIncidents(b *strings.Builder, p painter, r *result.Result) {
	a := r.Analysis
	if len(a.Incidents) > 0 {
		fmt.Fprintf(b, "  %s %d\n", p.paint(bold, pad("INCIDENTS", labelWidth)), len(a.Incidents))
		rows := [][]string{{"ID", "CLASS", "WINDOW", "HOT", "CULPRIT", "APP p99"}}
		for i, inc := range a.Incidents {
			if i == maxIncidentRows {
				break
			}
			var hot []string
			for _, rn := range inc.Runners {
				if rn.Hot {
					hot = append(hot, rn.Name)
				}
			}
			culprit := "-"
			if c := inc.Culprit; c != nil {
				switch {
				case len(c.TiedWith) > 0:
					culprit = "tie: " + strings.Join(append([]string{c.Runner}, c.TiedWith...), "/")
				case c.LeadBuckets != 0:
					culprit = fmt.Sprintf("%s (%+d)", c.Runner, c.LeadBuckets)
				default:
					culprit = c.Runner
				}
			}
			appP99 := "-"
			if inc.AppImpact != nil && inc.AppImpact.PeakP99MS > 0 {
				appP99 = render.MS(inc.AppImpact.PeakP99MS)
			}
			rows = append(rows, []string{
				inc.ID, inc.Class,
				fmt.Sprintf("%s-%s", render.Seconds(inc.StartS), render.Seconds(inc.EndS)),
				strings.Join(hot, ","), culprit, appP99,
			})
		}
		writeTable(b, p, rows, "    ")
		if n := len(a.Incidents) - maxIncidentRows; n > 0 {
			fmt.Fprintf(b, "    %s\n", p.paint(dim, fmt.Sprintf("... %d more in result.json", n)))
		}
		for _, inc := range a.Incidents[:min(len(a.Incidents), maxIncidentRows)] {
			for _, s := range inc.Telemetry {
				fmt.Fprintf(b, "    %s %s: %s %s %s -> %s\n", p.paint(dim, "·"), inc.ID,
					s.Source, s.Signal, render.Number(s.Baseline), render.Number(s.Value))
			}
		}
		b.WriteString("\n")
	}

	if len(a.Correlation) > 0 {
		parts := make([]string, 0, len(a.Correlation))
		for _, c := range a.Correlation {
			parts = append(parts, fmt.Sprintf("%s rho %.2f at lag %+d (%d buckets)", c.Storage, c.Rho, c.BestLag, c.NBuckets))
		}
		fmt.Fprintf(b, "  %s %s\n\n", p.paint(bold, pad("TRACKING", labelWidth)), p.paint(dim, strings.Join(parts, " · ")))
	}
}

func writeVerdict(b *strings.Builder, p painter, r *result.Result, opts Options) {
	v := r.Analysis.Verdict
	colour := cyan
	switch v.Bottleneck {
	case result.BottleneckNone:
		colour = green
	case result.BottleneckClient:
		colour = red
	}
	fmt.Fprintf(b, "  %s %s  %s\n",
		p.paint(bold, pad("VERDICT", labelWidth)),
		p.paint(colour, strings.ToUpper(v.Bottleneck)),
		p.paint(dim, "confidence: "+v.Confidence))
	if v.Summary != "" {
		fmt.Fprintf(b, "    %s\n", wrap(v.Summary, opts.Width-4, "    "))
	}
	for _, e := range v.Evidence {
		fmt.Fprintf(b, "    %s %s\n", p.paint(dim, "·"), wrap(e.Text, opts.Width-6, "      "))
	}
	if v.Caveat != "" {
		fmt.Fprintf(b, "    %s\n", p.paint(dim, v.Caveat))
	}
	for _, s := range v.NextSteps {
		fmt.Fprintf(b, "    %s %s\n", p.paint(cyan, "→"), wrap(s, opts.Width-6, "      "))
	}
	b.WriteString("\n")
}

// writeStrain reports, on a ramping run, the load at which latency began to degrade.
func writeStrain(b *strings.Builder, p painter, r *result.Result, opts Options) {
	s := r.Analysis.Strain
	if s == nil {
		return
	}
	colour := green
	if s.Found {
		colour = yellow
	}
	fmt.Fprintf(b, "  %s %s\n\n", p.paint(bold, pad("STRAIN", labelWidth)),
		p.paint(colour, wrap(s.Message, opts.Width-labelWidth-4, strings.Repeat(" ", labelWidth+3))))
}

// writeCapacity is a search's outcome and every level it ran.
func writeCapacity(b *strings.Builder, p painter, r *result.Result, opts Options) {
	c := r.Capacity
	if c == nil {
		return
	}
	fmt.Fprintf(b, "  %s %s\n\n", p.paint(bold, pad("CAPACITY", labelWidth)),
		wrap(c.Summary(), opts.Width-labelWidth-4, strings.Repeat(" ", labelWidth+3)))
	rows := [][]string{{"LEVEL", "PHASE", c.Knob, "HELD", "ACHIEVED/S", "P99", "CULPRIT"}}
	for _, l := range c.Levels {
		// Plain text: the table pads by byte length, so colour would skew its columns.
		held := "yes"
		if !l.OK {
			held = "no"
		}
		culprit := "-"
		if l.Culprit != nil {
			culprit = *l.Culprit
		}
		rows = append(rows, []string{strconv.Itoa(l.Level), l.Phase, render.Number(l.Value), held,
			render.Number(l.AchievedRPS), render.MS(l.P99MS), culprit})
	}
	writeTable(b, p, rows, "  ")
	b.WriteString("\n")
}

func writeArtifacts(b *strings.Builder, p painter, r *result.Result) {
	a := r.Artifacts
	if a == nil {
		return
	}
	for _, row := range [][2]string{
		{"result", a.Result}, {"report", a.Report}, {"events", a.Events}, {"digest", a.Digest},
	} {
		if row[1] != "" {
			fmt.Fprintf(b, "  %s %s\n", p.paint(dim, pad(row[0], 8)), row[1])
		}
	}
	b.WriteString("\n")
}

func formatErrors(m map[string]int64) string {
	counts := render.SortedCounts(m)
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("%s %d", c.Key, c.N))
	}
	return strings.Join(parts, ", ")
}

// writeTable prints rows in aligned columns, sizing each column to its widest cell.
func writeTable(b *strings.Builder, p painter, rows [][]string, indent string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for r, row := range rows {
		line := &strings.Builder{}
		line.WriteString(indent)
		for i, cell := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			if i == len(row)-1 {
				line.WriteString(cell)
			} else {
				line.WriteString(pad(cell, widths[i]))
			}
		}
		text := strings.TrimRight(line.String(), " ")
		if r == 0 {
			text = indent + p.paint(dim, strings.TrimLeft(text, " "))
		}
		b.WriteString(text + "\n")
	}
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// wrap breaks prose to a width, indenting continuation lines.
func wrap(s string, width int, indent string) string {
	if width < 20 {
		width = 20
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	lines = append(lines, line)
	return strings.Join(lines, "\n"+indent)
}
