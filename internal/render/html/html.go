// Package html renders the single-file, fully offline HTML report (§5.8).
//
// The report is one file with everything inside it: the chart library is embedded
// with go:embed, the data sits in a JSON script block, and a Content-Security-Policy
// of default-src 'none' permits exactly the inline scripts and styles it carries, by
// SHA-256 hashes computed from the bytes written. It therefore cannot make a network
// request, and a target that echoes markup into a URL or error cannot make it run
// anything.
//
// Like every renderer it is a pure function of the result: the same document renders
// to the same bytes.
package html

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/render"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

var (
	//go:embed assets/report.tmpl
	pageTemplate string
	//go:embed assets/report.css
	appCSS string
	//go:embed assets/report.js
	appJS string
	//go:embed assets/locale.js
	localeJS string
	//go:embed assets/uplot/uPlot.iife.min.js
	uplotJS string
	//go:embed assets/uplot/uPlot.min.css
	uplotCSS string
	//go:embed assets/uplot/LICENSE
	uplotLicense string
)

// DefaultEmbedLimit is the size above which the full result is left out and only the
// display data embedded (§5.8).
const DefaultEmbedLimit = 5 << 20

// Options control rendering.
type Options struct {
	// ResultPath is where result.json lives, named when the result is too large to
	// embed. Defaults to the result's own artifact path.
	ResultPath string
	// EmbedLimit overrides DefaultEmbedLimit; tests use it.
	EmbedLimit int
}

// page is what the template sees.
type page struct {
	R              *result.Result
	Title          string
	CSP            string
	UplotCSS       template.CSS
	AppCSS         template.CSS
	LocaleJS       template.JS
	UplotJS        template.JS
	AppJS          template.JS
	View           template.JS
	Full           template.JS
	FullBytes      int
	EmbedLimit     int
	ResultPath     string
	License        string
	Grouped        int
	StandingCaveat string
	SameHost       bool
	Samplers       []sampler
}

type sampler struct {
	Name          string
	Available     bool
	Reason        string
	StatementKeys []string
	Statements    [][]string
}

type qrow struct {
	Name string
	Q    result.Quantiles
}

var funcs = template.FuncMap{
	"ms": render.MS,
	"dms": func(v float64) string {
		if v < 0 {
			return "−" + render.MS(-v)
		}
		return "+" + render.MS(v)
	},
	"secs":   render.Seconds,
	"num":    render.Number,
	"pct":    render.Percent,
	"counts": render.SortedCounts,
	"join":   strings.Join,
	"div":    func(a, b float64) float64 { return a / b },
	"row":    func(name string, q result.Quantiles) qrow { return qrow{name, q} },
	"deref": func(v any) any {
		switch p := v.(type) {
		case *float64:
			return *p
		case *string:
			return *p
		}
		return v
	},
	"budget": func(metric string, v float64) string {
		if metric == "error_rate" {
			return render.Percent(v)
		}
		return render.MS(v)
	},
	"bytes": func(n int) string {
		switch {
		case n >= 1<<20:
			return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
		case n >= 1<<10:
			return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
		default:
			return fmt.Sprintf("%d bytes", n)
		}
	},
}

var tmpl = template.Must(template.New("report").Funcs(funcs).Parse(pageTemplate))

// Render writes the report for a result.
func Render(r *result.Result, opts Options) ([]byte, error) {
	if opts.EmbedLimit <= 0 {
		opts.EmbedLimit = DefaultEmbedLimit
	}
	if opts.ResultPath == "" && r.Artifacts != nil {
		opts.ResultPath = r.Artifacts.Result
	}

	v := buildView(r)
	viewJSON, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "encoding the report's display data")
	}
	// encoding/json escapes <, > and & inside strings, so a value can never close the
	// script element it sits in.
	full, err := json.Marshal(r)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "encoding the result for the report")
	}

	// The licence travels inside the script it covers, as MIT requires of a copy.
	uplot := "/*! uPlot 1.6.32 - MIT licence, reproduced in full:\n" + uplotLicense + "*/\n" + uplotJS

	p := page{
		R: r, Title: title(r),
		UplotCSS: template.CSS(uplotCSS), AppCSS: template.CSS(appCSS), //nolint:gosec // embedded assets, not input
		LocaleJS: template.JS(localeJS), UplotJS: template.JS(uplot), AppJS: template.JS(appJS), //nolint:gosec // embedded assets, not input
		View:       template.JS(viewJSON), //nolint:gosec // encoding/json output with HTML escaping
		FullBytes:  len(full),
		EmbedLimit: opts.EmbedLimit,
		ResultPath: opts.ResultPath,
		License:    uplotLicense,
		Grouped:    v.Grouped,

		StandingCaveat: result.StandingCaveat,
		SameHost:       sameHost(r),
		Samplers:       samplers(r),
	}
	if len(full) <= opts.EmbedLimit {
		p.Full = template.JS(full) //nolint:gosec // encoding/json output with HTML escaping
	}
	p.CSP = csp([]string{localeJS, uplot, appJS}, []string{uplotCSS, appCSS})

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "rendering the report")
	}
	return buf.Bytes(), nil
}

// csp allows nothing but the given inline scripts and styles. Hashes are computed
// from the exact text placed between the tags, so the policy cannot drift from the
// content.
func csp(scripts, styles []string) string {
	hashes := func(items []string) string {
		out := make([]string, 0, len(items))
		for _, s := range items {
			sum := sha256.Sum256([]byte(s))
			out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		return strings.Join(out, " ")
	}
	return "default-src 'none'; script-src " + hashes(scripts) + "; style-src " + hashes(styles) +
		"; base-uri 'none'; form-action 'none'"
}

func title(r *result.Result) string {
	name := r.Run.Name
	if name == "" {
		name = r.Run.ID
	}
	return fmt.Sprintf("%s - %s, %s - TracePoint", name, r.Analysis.Validity.State, r.Analysis.Verdict.Bottleneck)
}

func sameHost(r *result.Result) bool {
	for _, rn := range r.Runners {
		for _, t := range rn.Targets {
			if t.Scope == result.ScopeLoopback {
				return true
			}
		}
	}
	return false
}

// maxStatements bounds the statements table: it is context, and the result holds all.
const maxStatements = 20

func samplers(r *result.Result) []sampler {
	t := r.Telemetry
	if t == nil {
		return nil
	}
	var out []sampler
	if t.Generator != nil {
		out = append(out, sampler{Name: "generator", Available: len(t.Generator.Samples) > 0, Reason: "no samples"})
	}
	for _, s := range []struct {
		name   string
		series *result.SamplerSeries
	}{{"postgres", t.Postgres}, {"mysql", t.MySQL}, {"redis", t.Redis}} {
		if s.series == nil {
			continue
		}
		sm := sampler{Name: s.name, Available: s.series.Available, Reason: s.series.Reason}
		sm.StatementKeys, sm.Statements = statementTable(s.series.Statements)
		out = append(out, sm)
	}
	return out
}

func statementTable(rows []map[string]any) ([]string, [][]string) {
	if len(rows) == 0 {
		return nil, nil
	}
	keySet := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			keySet[k] = true
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var table [][]string
	for i, row := range rows {
		if i == maxStatements {
			break
		}
		cells := make([]string, len(keys))
		for j, k := range keys {
			if val, ok := row[k]; ok && val != nil {
				if n, isNum := number(val); isNum {
					cells[j] = render.Number(n)
				} else {
					cells[j] = fmt.Sprint(val)
				}
			}
		}
		table = append(table, cells)
	}
	return keys, table
}
