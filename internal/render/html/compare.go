package html

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"

	"github.com/IshaanNene/Tracepoint/internal/compare"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

var (
	//go:embed assets/compare.tmpl
	compareTemplate string
	//go:embed assets/compare.js
	compareJS string
)

var compareTmpl = template.Must(template.New("compare").Funcs(funcs).Parse(compareTemplate))

// comparePage is what the comparison template sees.
type comparePage struct {
	C        *compare.Report
	Title    string
	CSP      string
	UplotCSS template.CSS
	AppCSS   template.CSS
	LocaleJS template.JS
	UplotJS  template.JS
	AppJS    template.JS
	View     template.JS
	License  string
	Caveat   string
}

// compareView is the display data: each runner's p99 over time in both runs, each
// from its own start, so the curves overlay.
type compareView struct {
	Runners []compareRunner `json:"runners"`
}

type compareRunner struct {
	Name     string      `json:"name"`
	Baseline compareLine `json:"baseline"`
	Current  compareLine `json:"current"`
}

type compareLine struct {
	X []float64  `json:"x"`
	Y []*float64 `json:"y"`
}

// RenderCompare writes a comparison as one offline page, under the same policy as the
// run report: nothing can load and nothing from either run can execute.
func RenderCompare(rep *compare.Report, baseline, current *result.Result) ([]byte, error) {
	var v compareView
	for _, d := range rep.Runners {
		v.Runners = append(v.Runners, compareRunner{
			Name:     d.Runner,
			Baseline: line(baseline.RunnerByName(d.Runner), baseline.Run.BucketMS),
			Current:  line(current.RunnerByName(d.Runner), current.Run.BucketMS),
		})
	}
	viewJSON, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "encoding the comparison's display data")
	}
	uplot := "/*! uPlot 1.6.32 - MIT licence, reproduced in full:\n" + uplotLicense + "*/\n" + uplotJS
	p := comparePage{
		C:        rep,
		Title:    "TracePoint comparison: " + rep.Baseline.ID + " vs " + rep.Current.ID,
		UplotCSS: template.CSS(uplotCSS), AppCSS: template.CSS(appCSS), //nolint:gosec // embedded assets, not input
		LocaleJS: template.JS(localeJS), UplotJS: template.JS(uplot), AppJS: template.JS(compareJS), //nolint:gosec // embedded assets, not input
		View:    template.JS(viewJSON), //nolint:gosec // encoding/json output with HTML escaping
		License: uplotLicense,
		Caveat:  result.StandingCaveat,
	}
	p.CSP = csp([]string{localeJS, uplot, compareJS}, []string{uplotCSS, appCSS})
	var buf bytes.Buffer
	if err := compareTmpl.Execute(&buf, p); err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "rendering the comparison")
	}
	return buf.Bytes(), nil
}

// line is one runner's p99 response time per eligible bucket, thinned for display.
func line(rn *result.Runner, bucketMS float64) compareLine {
	if rn == nil {
		return compareLine{}
	}
	var x []float64
	var y []*float64
	for _, b := range rn.Buckets {
		x = append(x, (float64(b.Index)*bucketMS)/1000)
		if b.Warmup || b.Insufficient || b.N == 0 {
			y = append(y, nil)
			continue
		}
		v := b.Response.P99
		y = append(y, &v)
	}
	x, ys, _ := downsample(x, [][]*float64{y}, maxPoints)
	return compareLine{X: x, Y: ys[0]}
}
