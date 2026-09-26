package report

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/compare"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/render/cli"
	"github.com/IshaanNene/Tracepoint/internal/render/html"
	"github.com/IshaanNene/Tracepoint/internal/render/junit"
	"github.com/IshaanNene/Tracepoint/internal/render/markdown"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Comparison formats (spec §5.7). JSON is the comparison document itself.
const (
	Table = "table"
	JSON  = "json"
)

// CompareFormats are the comparison formats, in the order `compare --help` lists them.
var CompareFormats = []string{Table, JSON, Markdown, JUnit, HTML}

// NormaliseCompare accepts the comparison formats and their common aliases.
func NormaliseCompare(format string) (string, error) {
	switch f := strings.ToLower(strings.TrimSpace(format)); f {
	case "", Table, "text":
		return Table, nil
	case JSON:
		return JSON, nil
	}
	f, err := Normalise(format)
	if err != nil {
		return "", errs.New(errs.CodeOpsInvalidInput, "unknown comparison format %q", format).
			WithHint("use %s", strings.Join(CompareFormats, ", "))
	}
	return f, nil
}

// CompareFileName is where a comparison of a format is written by default.
func CompareFileName(format string) string {
	switch format {
	case JSON:
		return "compare.json"
	case Markdown:
		return "compare.md"
	case JUnit:
		return "compare-junit.xml"
	case HTML:
		return "compare.html"
	default:
		return "compare.txt"
	}
}

// RenderCompare renders a comparison. The HTML page needs both results for their
// timelines; every other format is a function of the report alone.
func RenderCompare(rep *compare.Report, baseline, current *result.Result, format string, width int) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	switch format {
	case Table:
		err = cli.RenderCompare(&buf, rep, cli.Options{Width: width})
	case JSON:
		var b []byte
		b, err = json.MarshalIndent(rep, "", "  ")
		buf.Write(append(b, '\n'))
	case Markdown:
		err = markdown.RenderCompare(&buf, rep)
	case JUnit:
		err = junit.RenderCompare(&buf, rep)
	case HTML:
		return html.RenderCompare(rep, baseline, current)
	default:
		_, err = NormaliseCompare(format)
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
