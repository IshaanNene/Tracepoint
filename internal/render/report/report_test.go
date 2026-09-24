package report_test

import (
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/analysis"
	"github.com/IshaanNene/Tracepoint/internal/render/report"
	"github.com/IshaanNene/Tracepoint/internal/result/resulttest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestFormats(t *testing.T) {
	for in, want := range map[string]string{"": "html", "HTML": "html", "md": "markdown", "markdown": "markdown", "xml": "junit", "junit": "junit"} {
		if got, err := report.Normalise(in); err != nil || got != want {
			t.Errorf("Normalise(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := report.Normalise("pdf"); err == nil {
		t.Fatal("pdf was accepted")
	}
	r := resulttest.NewRun(10).Analysed(analysis.Inputs{})
	for format, prefix := range map[string]string{report.HTML: "<!DOCTYPE html>", report.Markdown: "### TracePoint", report.JUnit: "<?xml"} {
		b, err := report.Render(r, format, "")
		if err != nil || !strings.HasPrefix(string(b), prefix) {
			t.Errorf("%s: %v %.40s", format, err, b)
		}
	}
	if report.FileName(report.JUnit) != "junit.xml" || report.FileName(report.Markdown) != "report.md" || report.FileName(report.HTML) != "report.html" {
		t.Fatal("file names")
	}
}
