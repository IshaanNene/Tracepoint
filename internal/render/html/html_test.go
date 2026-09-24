package html

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	stdhtml "html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/analysis"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/result/resulttest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// hostile is what a target might echo into a URL, a header or an error (§8).
const hostile = `</script><img src=x onerror=alert(1)>`

// fullResult exercises every section: an incident, a ramp, warm-up, telemetry,
// statements, labels, HTTP detail, capacity - and hostile strings wherever a target
// or a user could put text.
func fullResult(t *testing.T) *result.Result {
	t.Helper()
	r := resulttest.NewRun(60).Fault("db", 20, 24, 400).Fault("http", 21, 25, 900).Analysed(analysis.Inputs{})
	r.Run.Name = "checkout " + hostile
	r.Run.WarmupMS = 5000
	start := 0.0
	http := r.RunnerByName("http")
	http.Executor.StartTarget = &start
	http.Executor.Stages = []result.Stage{{DurationMS: 10_000, Target: 100}, {DurationMS: 50_000, Target: 100}}
	http.Targets = []result.Target{{Host: "127.0.0.1" + hostile, Scope: result.ScopeLoopback}}
	http.LabelSummaries = map[string]result.OpSummary{"/items?q=" + hostile: {N: 10, Errors: map[string]int64{"http_5xx" + hostile: 2}}}
	http.Summary.Errors = map[string]int64{"timeout": 3}
	http.HTTP = &result.HTTPDetail{
		StatusHistogram: map[string]int64{"200": 5990, "503": 10},
		PhasesMS:        map[string]result.Quantiles{"dns": {P99: 0.2}, "ttfb": {P99: 18}},
	}
	r.Warnings = []result.Finding{{Code: "SAME_HOST", Severity: result.SeverityWarn, Message: "target " + hostile}}
	r.Telemetry = &result.Telemetry{
		Generator: &result.GeneratorSeries{Samples: []map[string]any{{"t_ms": 0.0, "goroutines": 12}, {"t_ms": 1000.0, "goroutines": int64(14)}}},
		Postgres: &result.SamplerSeries{Available: true, IntervalMS: 1000,
			Samples:    []map[string]any{{"t_ms": 0.0, "lock_waits": 0.0, "state": "ok"}, {"t_ms": 1000.0, "lock_waits": 3.0}},
			Statements: []map[string]any{{"query": "SELECT " + hostile, "calls": 12.0}}},
		Redis: &result.SamplerSeries{Available: false, Reason: "INFO refused"},
	}
	culprit := "db"
	r.Capacity = &result.Capacity{Knob: "rate", Runner: "http",
		Levels: []result.CapacityLevel{
			{Level: 2, Value: 200, OK: false, BreakReason: "slo", P99MS: 900, AchievedRPS: 180, Culprit: &culprit},
			{Level: 1, Value: 100, OK: true, P99MS: 20, AchievedRPS: 100},
		},
		Boundary: &result.Boundary{LastOK: 100, FirstBroken: 200, Stable: true},
	}
	r.Artifacts = &result.Artifacts{RunDir: "runs/x", Result: "runs/x/result.json"}
	r.RunnerByName("db").Buckets[40].Insufficient = true
	return r
}

func renderHTML(t *testing.T, r *result.Result, opts Options) string {
	t.Helper()
	b, err := Render(r, opts)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRenderIsDeterministic(t *testing.T) {
	a := renderHTML(t, fullResult(t), Options{})
	b := renderHTML(t, fullResult(t), Options{})
	if a != b {
		t.Fatal("the same result rendered to different bytes")
	}
}

var (
	cspRe    = regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]*)">`)
	scriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	styleRe  = regexp.MustCompile(`(?s)<style>(.*?)</style>`)
	hashRe   = regexp.MustCompile(`'sha256-([A-Za-z0-9+/=]+)'`)
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// The policy allows exactly the inline code the file carries, and nothing else can
// run: no inline handlers, no style attributes, no other sources.
func TestCSPCoversExactlyTheInlineContent(t *testing.T) {
	out := renderHTML(t, fullResult(t), Options{})
	m := cspRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no CSP meta tag")
	}
	policy := stdhtml.UnescapeString(m[1])
	if !strings.HasPrefix(policy, "default-src 'none'; ") || !strings.Contains(policy, "base-uri 'none'") || !strings.Contains(policy, "form-action 'none'") {
		t.Fatalf("policy = %s", policy)
	}
	if strings.Index(out, "<meta http-equiv") > strings.Index(out, "<style>") {
		t.Fatal("the policy must come before any style or script")
	}
	directive := func(name string) []string {
		for _, d := range strings.Split(policy, ";") {
			if d = strings.TrimSpace(d); strings.HasPrefix(d, name+" ") {
				var hs []string
				for _, h := range hashRe.FindAllStringSubmatch(d, -1) {
					hs = append(hs, h[1])
				}
				sort.Strings(hs)
				return hs
			}
		}
		t.Fatalf("no %s directive", name)
		return nil
	}
	actual := func(re *regexp.Regexp) []string {
		var hs []string
		for _, s := range re.FindAllStringSubmatch(out, -1) {
			hs = append(hs, sha(s[1]))
		}
		sort.Strings(hs)
		return hs
	}
	if got, want := directive("script-src"), actual(scriptRe); strings.Join(got, " ") != strings.Join(want, " ") || len(want) != 3 {
		t.Fatalf("script hashes %v, inline scripts hash to %v", got, want)
	}
	if got, want := directive("style-src"), actual(styleRe); strings.Join(got, " ") != strings.Join(want, " ") || len(want) != 2 {
		t.Fatalf("style hashes %v, inline styles hash to %v", got, want)
	}
	if loc := regexp.MustCompile(`<[a-z][^>]*\s(on[a-z]+|style)\s*=`).FindString(out); loc != "" {
		t.Fatalf("an inline handler or style attribute: %s", loc)
	}
}

// §13: the report opens offline with zero network requests. Nothing in it can fetch:
// no tag that loads, no attribute that points outside the page, no CSS url().
func TestNoExternalReferences(t *testing.T) {
	out := renderHTML(t, fullResult(t), Options{})
	for _, tag := range []string{"<img", "<link", "<iframe", "<object", "<embed", "<base", "<audio", "<video", "<source", "<frame", `http-equiv="refresh"`} {
		if strings.Contains(strings.ToLower(out), tag) {
			t.Errorf("the report contains %s", tag)
		}
	}
	attr := regexp.MustCompile(`(?i)\s(src|href|action|formaction|poster|srcset|data|background|ping|manifest|xlink:href|cite)\s*=\s*"([^"]*)"`)
	for _, m := range attr.FindAllStringSubmatch(out, -1) {
		if !strings.HasPrefix(m[2], "#") {
			t.Errorf("%s=%q points outside the page", m[1], m[2])
		}
	}
	for _, s := range styleRe.FindAllStringSubmatch(out, -1) {
		if strings.Contains(s[1], "url(") || strings.Contains(s[1], "@import") {
			t.Error("a stylesheet loads something")
		}
	}
	for _, s := range scriptRe.FindAllStringSubmatch(out, -1) {
		for _, api := range []string{"fetch(", "XMLHttpRequest", "WebSocket", "EventSource", "sendBeacon", "import(", "importScripts", ".src="} {
			if strings.Contains(s[1], api) {
				t.Errorf("a script uses %s", api)
			}
		}
	}
}

// §8: a URL containing </script><img src=x onerror=alert(1)> renders inert.
func TestHostileStringsRenderInert(t *testing.T) {
	out := renderHTML(t, fullResult(t), Options{})
	if strings.Contains(out, "<img") {
		t.Fatal("hostile markup reached the document as markup")
	}
	opened := strings.Count(out, "<script")
	if closed := strings.Count(out, "</script>"); closed != opened {
		t.Fatalf("%d script elements opened, %d closed: a value ended one early", opened, closed)
	}
	if !strings.Contains(out, "checkout &lt;/script&gt;&lt;img src=x onerror=alert(1)&gt;") {
		t.Fatal("the hostile run name should appear escaped, as text")
	}
	for _, id := range []string{"tp-view", "tp-result"} {
		block := regexp.MustCompile(`(?s)<script type="application/json" id="` + id + `">(.*?)</script>`).FindStringSubmatch(out)
		if len(block) < 2 || !json.Valid([]byte(block[1])) {
			t.Fatalf("the %s block is missing or not JSON", id)
		}
	}
}

// Above the embed limit, only display data is embedded, and the report says where the
// full result is.
func TestLargeResultEmbedsDisplayDataOnly(t *testing.T) {
	out := renderHTML(t, fullResult(t), Options{EmbedLimit: 1024, ResultPath: "runs/big/result.json"})
	if strings.Contains(out, `id="tp-result"`) || strings.Contains(out, `id="export"`) {
		t.Fatal("the full result was embedded above the limit")
	}
	if !strings.Contains(out, "<code>runs/big/result.json</code>") || !strings.Contains(out, `id="tp-view"`) {
		t.Fatal("the report must say where result.json lives and keep its display data")
	}
	small := renderHTML(t, fullResult(t), Options{})
	if !strings.Contains(small, `id="tp-result"`) || !strings.Contains(small, `id="export"`) {
		t.Fatal("a small result should be embedded with an export button")
	}
}

func TestLongRunsAreThinnedForDisplayOnly(t *testing.T) {
	r := resulttest.NewRun(5000).Fault("db", 4000, 4000, 777).Analysed(analysis.Inputs{})
	v := buildView(r)
	if len(v.X) > maxPoints || v.Grouped != 3 {
		t.Fatalf("%d points, grouped %d", len(v.X), v.Grouped)
	}
	peak := 0.0
	for _, p := range v.Runners[1].Service["p99"] {
		if p != nil && *p > peak {
			peak = *p
		}
	}
	if peak != 777 {
		t.Fatalf("a one-bucket spike of 777ms displayed as %v", peak)
	}
	if out := renderHTML(t, r, Options{}); !strings.Contains(out, "maximum of 3 buckets") {
		t.Fatal("the report must say the timeline was thinned")
	}
}

func TestBandsAndGaps(t *testing.T) {
	r := fullResult(t)
	r.RunnerByName("db").Buckets[3].Insufficient = true
	v := buildView(r)
	kinds := map[string]int{}
	for _, b := range v.Bands {
		kinds[b.Kind]++
	}
	if kinds[bandWarmup] != 1 || kinds[bandRamp] != 1 || kinds[bandIncident] != len(r.Analysis.Incidents) || kinds[bandIncident] == 0 {
		t.Fatalf("bands = %+v", v.Bands)
	}
	if v.Runners[1].Service["p99"][3] != nil || v.Runners[1].Sparse["service.p99"][3] == nil {
		t.Fatal("an insufficient bucket must leave the line and be drawn apart from it")
	}
	if v.Runners[1].Sparse["service.p99"][4] != nil {
		t.Fatal("a sufficient bucket must not appear among the sparse points")
	}
	// A 1.0 result cannot tell a ramp from a hold, so it shades none.
	r.RunnerByName("http").Executor.StartTarget = nil
	for _, b := range buildView(r).Bands {
		if b.Kind == bandRamp {
			t.Fatal("a ramp was guessed without start_target")
		}
	}
}

func TestInvalidRunsSayNotToReadTheVerdict(t *testing.T) {
	r := fullResult(t)
	r.Analysis.Validity.State = result.ValidityInvalid
	if out := renderHTML(t, r, Options{}); !strings.Contains(out, "This run is invalid.") {
		t.Fatal("no warning on an invalid run")
	}
}

// The vendored library is pinned: replacing it is a reviewed change (PROVENANCE.md).
func TestVendoredAssetsArePinned(t *testing.T) {
	for file, want := range map[string]string{
		"uPlot.iife.min.js": "19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f",
		"uPlot.min.css":     "df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04",
	} {
		b, err := os.ReadFile(filepath.Join("assets", "uplot", file))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s is %s, PROVENANCE.md pins %s", file, got, want)
		}
	}
}

// chromium finds a headless browser, or skips: the structural tests above hold without
// one, and this proves the same in a real engine where one is installed.
func chromium(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TRACEPOINT_CHROMIUM"); p != "" {
		return p
	}
	if m, _ := filepath.Glob("/opt/pw-browsers/chromium_headless_shell-*/chrome-linux/headless_shell"); len(m) > 0 {
		return m[0]
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "headless_shell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no headless Chromium; set TRACEPOINT_CHROMIUM to run the browser check")
	return ""
}

// In a real browser: every script runs under the policy (no violation, no error), the
// charts draw, and the hostile strings stay text. Any request the page attempted would
// be refused by default-src 'none' and logged, so a silent console also means no
// request was made.
func TestInABrowser(t *testing.T) {
	browser := chromium(t)
	path := filepath.Join(t.TempDir(), "report.html")
	if err := os.WriteFile(path, []byte(renderHTML(t, fullResult(t), Options{})), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, browser, "--headless", "--no-sandbox", "--disable-gpu",
		"--enable-logging=stderr", "--v=0", "--virtual-time-budget=5000", "--dump-dom", "file://"+path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", browser, err, stderr.String())
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(line, ":CONSOLE") {
			t.Errorf("console: %s", line)
		}
	}
	dom := stdout.String()
	// The timeline, the postgres and generator telemetry, and the capacity chart.
	if n := strings.Count(dom, "<canvas"); n != 4 {
		t.Errorf("%d charts drew, want 4", n)
	}
	if strings.Contains(dom, "<img") {
		t.Error("hostile markup became an element")
	}
}
