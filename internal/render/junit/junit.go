// Package junit renders a run as JUnit XML, for CI systems that display test results.
//
// The test cases mirror the exit codes, so CI and the process agree: the run itself
// (an error when it aborted, failed or was cut short), validity (a failure when
// invalid, since an invalid run proves nothing about the target) and one case per SLO
// check (a failure when breached). Incidents and the verdict are context, carried in
// the suite's system-out; they fail nothing on their own.
//
// Like every renderer it is a pure function of the result.
package junit

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/render"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

type suites struct {
	XMLName  xml.Name `xml:"testsuites"`
	Name     string   `xml:"name,attr"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Errors   int      `xml:"errors,attr"`
	Time     string   `xml:"time,attr"`
	Suites   []suite  `xml:"testsuite"`
}

type suite struct {
	Name       string     `xml:"name,attr"`
	Tests      int        `xml:"tests,attr"`
	Failures   int        `xml:"failures,attr"`
	Errors     int        `xml:"errors,attr"`
	Skipped    int        `xml:"skipped,attr"`
	Time       string     `xml:"time,attr"`
	Timestamp  string     `xml:"timestamp,attr,omitempty"`
	Properties []property `xml:"properties>property"`
	Cases      []testcase `xml:"testcase"`
	SystemOut  string     `xml:"system-out,omitempty"`
}

type property struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type testcase struct {
	Name      string   `xml:"name,attr"`
	Classname string   `xml:"classname,attr"`
	Time      string   `xml:"time,attr"`
	Failure   *outcome `xml:"failure"`
	Error     *outcome `xml:"error"`
	SystemOut string   `xml:"system-out,omitempty"`
}

type outcome struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// Render writes the JUnit document.
func Render(w io.Writer, r *result.Result) error {
	a := r.Analysis
	elapsed := r.Run.ElapsedMS
	if elapsed == 0 {
		elapsed = r.Run.DurationMS
	}
	secs := fmt.Sprintf("%.3f", elapsed/1000)
	zero := "0.000"

	name := r.Run.Name
	if name == "" {
		name = r.Run.ID
	}
	s := suite{
		Name: "tracepoint." + name, Time: secs, Timestamp: r.Run.StartedAt,
		Properties: []property{
			{"run_id", r.Run.ID}, {"seed", fmt.Sprint(r.Run.Seed)}, {"status", r.Run.Status},
			{"validity", a.Validity.State}, {"bottleneck", a.Verdict.Bottleneck},
			{"confidence", a.Verdict.Confidence}, {"tool_version", r.Tool.Version},
			{"result_schema", r.SchemaVersion},
		},
	}
	if r.Artifacts != nil && r.Artifacts.RunDir != "" {
		s.Properties = append(s.Properties, property{"run_dir", r.Artifacts.RunDir})
	}

	run := testcase{Name: "run " + r.Run.Status, Classname: "tracepoint.run", Time: secs}
	if r.Run.Status != result.StatusCompleted {
		code := errs.CodeRunInterrupted
		switch r.Run.Status {
		case result.StatusAborted:
			code = errs.CodeRunAborted
		case result.StatusFailed:
			code = errs.CodeRunFailed
		}
		msg := "the run " + r.Run.Status
		if r.Run.InterruptedReason != "" {
			msg += ": " + r.Run.InterruptedReason
		}
		run.Error = &outcome{Message: msg, Type: string(code), Text: msg}
	}
	s.Cases = append(s.Cases, run)

	validity := testcase{Name: "validity " + a.Validity.State, Classname: "tracepoint.validity", Time: zero}
	var lines []string
	for _, f := range a.Validity.Findings {
		line := fmt.Sprintf("%s %s: %s", f.Severity, f.Code, f.Message)
		if f.Fix != "" {
			line += " Fix: " + f.Fix
		}
		lines = append(lines, line)
	}
	if a.Validity.State == result.ValidityInvalid {
		validity.Failure = &outcome{
			Message: "the run is invalid: the load generator, not the target, set the pace",
			Type:    string(errs.CodeRunInvalid), Text: strings.Join(lines, "\n"),
		}
	} else if len(lines) > 0 {
		validity.SystemOut = strings.Join(lines, "\n")
	}
	s.Cases = append(s.Cases, validity)

	for _, c := range a.SLO.Checks {
		tc := testcase{
			Name:      fmt.Sprintf("%s %s <= %s", c.Runner, c.Metric, budget(c.Metric, c.Budget)),
			Classname: "tracepoint.slo." + c.Runner, Time: zero,
		}
		if !c.Pass {
			msg := fmt.Sprintf("%s %s was %s, over its budget of %s", c.Runner, c.Metric, budget(c.Metric, c.Actual), budget(c.Metric, c.Budget))
			tc.Failure = &outcome{Message: msg, Type: "SLO_BREACH", Text: msg}
		}
		s.Cases = append(s.Cases, tc)
	}

	s.SystemOut = systemOut(r)
	for _, tc := range s.Cases {
		s.Tests++
		if tc.Failure != nil {
			s.Failures++
		}
		if tc.Error != nil {
			s.Errors++
		}
	}
	doc := suites{Name: "tracepoint", Tests: s.Tests, Failures: s.Failures, Errors: s.Errors, Time: secs, Suites: []suite{s}}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "encoding JUnit XML")
	}
	if _, werr := io.WriteString(w, xml.Header); werr != nil {
		return werr
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

// systemOut is the verdict and incidents, as a reader of a CI log wants them.
func systemOut(r *result.Result) string {
	a := r.Analysis
	b := &strings.Builder{}
	fmt.Fprintf(b, "Verdict: %s (bottleneck %s, confidence %s)\n", a.Verdict.Summary, a.Verdict.Bottleneck, a.Verdict.Confidence)
	for _, inc := range a.Incidents {
		fmt.Fprintf(b, "Incident %s: %s, %s to %s", inc.ID, inc.Class, render.Seconds(inc.StartS), render.Seconds(inc.EndS))
		if inc.Culprit != nil {
			fmt.Fprintf(b, ", culprit %s", inc.Culprit.Runner)
		}
		b.WriteString("\n")
	}
	for _, rn := range r.Runners {
		fmt.Fprintf(b, "%s: %d operations, %s errors, p99 %s\n", rn.Name, rn.Summary.N, render.Percent(rn.Summary.ErrorRatio), render.MS(rn.Summary.Response.P99))
	}
	b.WriteString(result.StandingCaveat)
	return b.String()
}

func budget(metric string, v float64) string {
	if metric == "error_rate" {
		return render.Percent(v)
	}
	return render.MS(v)
}
