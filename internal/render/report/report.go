// Package report renders a result in any of the report formats, so the `report`
// command and the render_report operation cannot disagree about what a format is or
// what it is called on disk.
package report

import (
	"bytes"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/render/html"
	"github.com/IshaanNene/Tracepoint/internal/render/junit"
	"github.com/IshaanNene/Tracepoint/internal/render/markdown"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Formats.
const (
	HTML     = "html"
	Markdown = "markdown"
	JUnit    = "junit"
)

// Formats lists every format, for help text and schemas.
var Formats = []string{HTML, Markdown, JUnit}

// Normalise accepts a format's common spellings.
func Normalise(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", HTML, "htm":
		return HTML, nil
	case Markdown, "md":
		return Markdown, nil
	case JUnit, "xml", "junit-xml":
		return JUnit, nil
	}
	return "", errs.New(errs.CodeOpsInvalidInput, "unknown report format %q", format).
		WithHint("use html, markdown or junit")
}

// FileName is the format's file name inside a run directory.
func FileName(format string) string {
	switch format {
	case Markdown:
		return "report.md"
	case JUnit:
		return "junit.xml"
	default:
		return "report.html"
	}
}

// Render renders a result. resultPath is named by the HTML report when the result is
// too large to embed.
func Render(r *result.Result, format, resultPath string) ([]byte, error) {
	switch format {
	case HTML:
		return html.Render(r, html.Options{ResultPath: resultPath})
	case Markdown:
		var buf bytes.Buffer
		err := markdown.Render(&buf, r, markdown.Options{})
		return buf.Bytes(), err
	case JUnit:
		var buf bytes.Buffer
		err := junit.Render(&buf, r)
		return buf.Bytes(), err
	}
	_, err := Normalise(format)
	return nil, err
}
