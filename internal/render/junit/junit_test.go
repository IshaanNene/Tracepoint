package junit_test

import (
	"bytes"
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/analysis"
	"github.com/IshaanNene/Tracepoint/internal/render/junit"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/result/resulttest"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func breached() *result.Result {
	r := resulttest.NewRun(60).Fault("db", 20, 24, 400).Fault("http", 21, 25, 900).Analysed(analysis.Inputs{})
	r.Analysis.SLO = result.SLO{Pass: false, Checks: []result.SLOCheck{
		{Runner: "http", Metric: "p99", Budget: 250, Actual: 900, Pass: false},
		{Runner: "http", Metric: "error_rate", Budget: 0.01, Actual: 0, Pass: true},
	}}
	return r
}

func render(t *testing.T, r *result.Result) string {
	t.Helper()
	var buf bytes.Buffer
	if err := junit.Render(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// decoded is the part of the format CI systems read.
type decoded struct {
	Tests    int `xml:"tests,attr"`
	Failures int `xml:"failures,attr"`
	Errors   int `xml:"errors,attr"`
	Suite    struct {
		Cases []struct {
			Name    string `xml:"name,attr"`
			Failure *struct {
				Type string `xml:"type,attr"`
			} `xml:"failure"`
			Error *struct {
				Type string `xml:"type,attr"`
			} `xml:"error"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

func decode(t *testing.T, s string) decoded {
	t.Helper()
	var d decoded
	if err := xml.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("not well-formed XML: %v\n%s", err, s)
	}
	return d
}

func TestGolden(t *testing.T) {
	got := render(t, breached())
	path := filepath.Join("testdata", "breached.golden.xml")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run `go test ./internal/render/junit/ -update` to create it", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s\n--- got ---\n%s", path, got)
	}
}

// Failures mirror the exit codes: an SLO breach and an invalid run fail, a run cut
// short is an error, and a clean run passes everything.
func TestOutcomesMirrorExitCodes(t *testing.T) {
	d := decode(t, render(t, breached()))
	if d.Tests != 4 || d.Failures != 1 || d.Errors != 0 {
		t.Fatalf("breach: %+v", d)
	}
	if f := d.Suite.Cases[2].Failure; f == nil || f.Type != "SLO_BREACH" {
		t.Fatalf("the p99 case: %+v", d.Suite.Cases[2])
	}

	r := breached()
	r.Analysis.Validity.State = result.ValidityInvalid
	r.Run.Status = result.StatusAborted
	d = decode(t, render(t, r))
	if d.Failures != 2 || d.Errors != 1 || d.Suite.Cases[0].Error.Type != "RUN_ABORTED" || d.Suite.Cases[1].Failure.Type != "RUN_INVALID" {
		t.Fatalf("aborted and invalid: %+v", d)
	}

	clean := resulttest.NewRun(10).Analysed(analysis.Inputs{})
	if d := decode(t, render(t, clean)); d.Failures != 0 || d.Errors != 0 || d.Tests != 2 {
		t.Fatalf("clean: %+v", d)
	}
}

// Markup and control characters from a target cannot break the document.
func TestHostileTextStaysWellFormed(t *testing.T) {
	r := breached()
	r.Run.Name = `x"/><testcase name="forged"/>` + "\x00\x1b[31m"
	r.Analysis.Validity.Findings = []result.Finding{{Code: "X", Severity: "warn", Message: "]]><evil/>&amp;"}}
	out := render(t, r)
	d := decode(t, out)
	if strings.Contains(out, "<evil/>") || strings.Contains(out, `<testcase name="forged"`) || d.Tests != 4 {
		t.Fatalf("hostile text escaped the document:\n%s", out)
	}
}
