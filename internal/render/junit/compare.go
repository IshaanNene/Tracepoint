package junit

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/compare"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// RenderCompare writes a comparison as JUnit XML: one test case per gate, failing
// exactly when the gate did, so the CI view and `compare`'s exit code agree.
func RenderCompare(w io.Writer, rep *compare.Report) error {
	s := suite{
		Name: "tracepoint.compare", Time: "0.000",
		Properties: []property{
			{Name: "baseline", Value: rep.Baseline.ID},
			{Name: "current", Value: rep.Current.ID},
			{Name: "regression", Value: fmt.Sprint(rep.Regression)},
			{Name: "compare_schema", Value: rep.SchemaVersion},
		},
	}
	for _, g := range rep.Gates {
		tc := testcase{Name: g.Runner + " " + g.Name, Classname: "tracepoint.compare." + g.Runner, Time: "0.000"}
		if !g.Pass {
			typ := string(errs.CodeCompareRegression)
			if g.Insufficient {
				typ = string(errs.CodeCompareInsufficient)
			}
			tc.Failure = &outcome{Message: g.Message, Type: typ, Text: g.Message}
			s.Failures++
		}
		s.Cases = append(s.Cases, tc)
		s.Tests++
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "%s\n", rep.Summary)
	for _, d := range rep.Runners {
		fmt.Fprintf(b, "%s p99 %.1fms -> %.1fms (%+.1f%%), %s\n", d.Runner, d.Baseline.P99MS, d.Current.P99MS, d.P99Pct, d.Change)
	}
	for _, inc := range rep.Incidents {
		fmt.Fprintf(b, "incident on %s: %s\n", inc.Runner, inc.Status)
	}
	for _, wn := range rep.Warnings {
		fmt.Fprintf(b, "%s: %s\n", wn.Code, wn.Message)
	}
	s.SystemOut = b.String()
	doc := suites{Name: "tracepoint", Tests: s.Tests, Failures: s.Failures, Time: "0.000", Suites: []suite{s}}
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
