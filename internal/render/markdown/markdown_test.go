package markdown_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/analysis"
	"github.com/IshaanNene/Tracepoint/internal/render/markdown"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/result/resulttest"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func dbIncident() *result.Result {
	r := resulttest.NewRun(60).Fault("db", 20, 24, 400).Fault("http", 21, 25, 900).Analysed(analysis.Inputs{})
	r.Analysis.SLO = result.SLO{Pass: false, Checks: []result.SLOCheck{
		{Runner: "http", Metric: "p99", Budget: 250, Actual: 900, Pass: false},
		{Runner: "http", Metric: "error_rate", Budget: 0.01, Actual: 0, Pass: true},
	}}
	r.Warnings = []result.Finding{{Code: "SAME_HOST", Severity: result.SeverityWarn, Message: "the generator and a target share a host", Fix: "run the generator elsewhere"}}
	r.Artifacts = &result.Artifacts{RunDir: "runs/20260924T100000Z-abc123"}
	return r
}

func render(t *testing.T, r *result.Result, opts markdown.Options) string {
	t.Helper()
	var buf bytes.Buffer
	if err := markdown.Render(&buf, r, opts); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
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
		t.Fatalf("%v; run `go test ./internal/render/markdown/ -update` to create it", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s\n--- got ---\n%s", path, got)
	}
}

func TestGolden(t *testing.T) {
	golden(t, "db-incident.golden.md", render(t, dbIncident(), markdown.Options{}))

	clean := resulttest.NewRun(30).Analysed(analysis.Inputs{})
	clean.Analysis.SLO.Pass = true
	golden(t, "clean.golden.md", render(t, clean, markdown.Options{}))
}

func TestInvalidRunsWarnFirst(t *testing.T) {
	r := dbIncident()
	r.Analysis.Validity.State = result.ValidityInvalid
	out := render(t, r, markdown.Options{})
	if !strings.Contains(out, "**Validity:** **invalid**") || !strings.Contains(out, "> [!WARNING]") {
		t.Fatalf("an invalid run must say so first:\n%s", out)
	}
	if strings.Index(out, "[!WARNING]") > strings.Index(out, r.Analysis.Verdict.Summary[:10]) {
		t.Fatal("the warning must precede the verdict")
	}
}

// A target's text cannot format, link, break a table, inject HTML or mention anyone.
func TestHostileTextIsLiteral(t *testing.T) {
	r := dbIncident()
	r.Run.Name = "x | y\n# heading <script>alert(1)</script> [click](https://evil.example) @octocat **bold**"
	r.Warnings = append(r.Warnings, result.Finding{Code: "X`Y", Severity: "warn", Message: "<img src=x onerror=alert(1)> @org/team"})
	out := render(t, r, markdown.Options{})
	for _, bad := range []string{"<script>", "<img", "](https", "@octocat", "@org", "\n# heading", "**bold**", "X`Y"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q survived unescaped", bad)
		}
	}
	title := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(title, "### TracePoint: x \\| y \\# heading &lt;script&gt;") {
		t.Fatalf("title = %s", title)
	}
}

func TestIncidentTableIsBounded(t *testing.T) {
	r := dbIncident()
	for len(r.Analysis.Incidents) < 4 {
		r.Analysis.Incidents = append(r.Analysis.Incidents, r.Analysis.Incidents[0])
	}
	out := render(t, r, markdown.Options{MaxIncidents: 2})
	if strings.Count(out, "| inc\\-1 |") != 2 || !strings.Contains(out, "2 more incident(s)") {
		t.Fatalf("incidents:\n%s", out)
	}
}

func TestStrainIsSummarised(t *testing.T) {
	r := dbIncident()
	r.Analysis.Strain = &result.Strain{Found: false, Users: 25, RPS: 240, Message: "no strain up to ~25 users (~240 req/s)"}
	if out := render(t, r, markdown.Options{}); !strings.Contains(out, "**Strain:** no strain up to \\~25 users") {
		t.Fatalf("markdown:\n%s", out)
	}
}

func TestCapacityLevels(t *testing.T) {
	r := resulttest.NewRun(30).Analysed(analysis.Inputs{})
	r.Capacity = &result.Capacity{Knob: "rate", Runner: "http",
		Levels: []result.CapacityLevel{
			{Level: 0, Phase: "doubling", Value: 200, OK: true, AchievedRPS: 199, P99MS: 13},
			{Level: 1, Phase: "doubling", Value: 400, AchievedRPS: 380, P99MS: 130, BreakReason: "http p99 130ms > 100ms | <b>x</b>"},
		},
		Boundary: &result.Boundary{LastOK: 200, FirstBroken: 400, Stable: true},
	}
	out := render(t, r, markdown.Options{})
	for _, want := range []string{"**Capacity:** Capacity holds at 200/s and breaks at 400/s", "| 1 | doubling | 400 | **no** |"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<b>") || strings.Contains(out, "100ms | <") {
		t.Fatalf("a break reason escaped its cell:\n%s", out)
	}
}
