// Package markdown renders a run as GitHub-flavoured Markdown, sized for a
// pull-request comment or a CI job summary.
//
// It leads with what a reviewer needs - validity, SLOs, the verdict - and keeps the
// detail in tables below. Every string that came from a user or a target is escaped:
// Markdown and HTML metacharacters become literal text, and an @ cannot mention
// anyone, so a hostile target cannot format, link or ping its way into a review.
//
// Like every renderer it is a pure function of the result.
package markdown

import (
	"fmt"
	"io"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/render"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Options control rendering.
type Options struct {
	// MaxIncidents bounds the incident table; the rest are counted. Zero means 10.
	MaxIncidents int
}

// Render writes the summary.
func Render(w io.Writer, r *result.Result, opts Options) error {
	if opts.MaxIncidents <= 0 {
		opts.MaxIncidents = 10
	}
	b := &strings.Builder{}
	a := r.Analysis

	name := r.Run.Name
	if name == "" {
		name = r.Run.ID
	}
	fmt.Fprintf(b, "### TracePoint: %s\n\n", esc(name))
	slo := "pass"
	if !a.SLO.Pass {
		slo = "**fail**"
	}
	validity := a.Validity.State
	if validity == result.ValidityInvalid {
		validity = "**invalid**"
	}
	fmt.Fprintf(b, "**Validity:** %s · **SLO:** %s · **Bottleneck:** %s · **Confidence:** %s", validity, slo, esc(a.Verdict.Bottleneck), esc(a.Verdict.Confidence))
	if r.Run.Status != result.StatusCompleted {
		fmt.Fprintf(b, " · **Run %s**", esc(r.Run.Status))
		if r.Run.InterruptedReason != "" {
			fmt.Fprintf(b, " (%s)", esc(r.Run.InterruptedReason))
		}
	}
	b.WriteString("\n\n")

	if a.Validity.State == result.ValidityInvalid {
		b.WriteString("> [!WARNING]\n> This run is invalid: the load generator, not the target, set the pace. Do not read the verdict as a finding about the system under test.\n\n")
	}

	v := a.Verdict
	if v.Summary != "" {
		fmt.Fprintf(b, "%s\n\n", esc(v.Summary))
	}
	if len(v.Evidence) > 0 {
		b.WriteString("<details><summary>Evidence</summary>\n\n")
		for _, e := range v.Evidence {
			fmt.Fprintf(b, "- `%s` %s\n", code(e.Kind), esc(e.Text))
		}
		b.WriteString("\n</details>\n\n")
	}
	if len(v.NextSteps) > 0 {
		b.WriteString("**Next steps**\n\n")
		for i, s := range v.NextSteps {
			fmt.Fprintf(b, "%d. %s\n", i+1, esc(s))
		}
		b.WriteString("\n")
	}
	if s := a.Strain; s != nil {
		fmt.Fprintf(b, "**Strain:** %s\n\n", esc(s.Message))
	}
	caveat := result.StandingCaveat
	if v.Caveat != "" && v.Caveat != caveat {
		caveat = v.Caveat + " " + caveat
	}
	fmt.Fprintf(b, "> %s\n\n", esc(caveat))

	if len(a.SLO.Checks) > 0 {
		b.WriteString("#### SLOs\n\n| Runner | Metric | Budget | Actual | Result |\n| --- | --- | ---: | ---: | --- |\n")
		for _, c := range a.SLO.Checks {
			res := "pass"
			if !c.Pass {
				res = "**fail**"
			}
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", esc(c.Runner), esc(c.Metric), budget(c.Metric, c.Budget), budget(c.Metric, c.Actual), res)
		}
		b.WriteString("\n")
	}

	b.WriteString("#### Runners\n\n| Runner | Operations | Errors | Achieved/s | p50 | p95 | p99 | p99 service |\n| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, rn := range r.Runners {
		s := rn.Summary
		fmt.Fprintf(b, "| %s | %d | %s | %s | %s | %s | %s | %s |\n", esc(rn.Name), s.N, render.Percent(s.ErrorRatio),
			render.Number(s.AchievedRPS), render.MS(s.Response.P50), render.MS(s.Response.P95), render.MS(s.Response.P99), render.MS(s.Service.P99))
	}
	b.WriteString("\n")

	if n := len(a.Incidents); n > 0 {
		b.WriteString("#### Incidents\n\n| Incident | Class | Window | Culprit | App peak p99 |\n| --- | --- | --- | --- | ---: |\n")
		for i, inc := range a.Incidents {
			if i == opts.MaxIncidents {
				break
			}
			culprit := "—"
			if inc.Culprit != nil {
				culprit = esc(inc.Culprit.Runner)
				if len(inc.Culprit.TiedWith) > 0 {
					culprit += " (tied with " + esc(strings.Join(inc.Culprit.TiedWith, ", ")) + ")"
				}
			}
			peak := "—"
			if inc.AppImpact != nil && inc.AppImpact.PeakP99MS > 0 {
				peak = render.MS(inc.AppImpact.PeakP99MS)
			}
			fmt.Fprintf(b, "| %s | %s | %s–%s | %s | %s |\n", esc(inc.ID), esc(inc.Class), render.Seconds(inc.StartS), render.Seconds(inc.EndS), culprit, peak)
		}
		if n > opts.MaxIncidents {
			fmt.Fprintf(b, "\n%d more incident(s) are in the report.\n", n-opts.MaxIncidents)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("No incidents.\n\n")
	}

	findings := append(append([]result.Finding(nil), a.Validity.Findings...), r.Warnings...)
	if len(findings) > 0 {
		b.WriteString("#### Findings\n\n")
		for _, f := range findings {
			fmt.Fprintf(b, "- **%s** `%s` %s", esc(f.Severity), code(f.Code), esc(f.Message))
			if f.Fix != "" {
				fmt.Fprintf(b, " — %s", esc(f.Fix))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(b, "<sub>run `%s` · %s · seed %d · TracePoint %s", code(r.Run.ID), render.Seconds(r.Run.DurationMS/1000), r.Run.Seed, esc(r.Tool.Version))
	if r.Artifacts != nil && r.Artifacts.RunDir != "" {
		fmt.Fprintf(b, " · %s", esc(r.Artifacts.RunDir))
	}
	b.WriteString("</sub>\n")

	_, err := io.WriteString(w, b.String())
	return err
}

func budget(metric string, v float64) string {
	if metric == "error_rate" {
		return render.Percent(v)
	}
	return render.MS(v)
}

// esc makes a string literal text in GitHub Markdown, inside or outside a table:
// markup characters are backslash-escaped, HTML is entity-encoded, line breaks become
// spaces so a value cannot start a new block, and a zero-width space after every @
// stops it mentioning a user or team.
func esc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '`', '*', '_', '[', ']', '(', ')', '#', '+', '-', '!', '|', '~', '{', '}', '.', ':':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '@':
			b.WriteString("@\u200b")
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			if r < 0x20 || r == 0x7f {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// code is for identifiers TracePoint defines (codes, kinds, run ids): they go inside a
// code span, so a backtick is the one character that must not survive.
func code(s string) string {
	return strings.NewReplacer("`", "'", "\n", " ", "\r", " ").Replace(s)
}
