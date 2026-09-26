package html

import (
	"bytes"
	"context"
	stdhtml "html"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/analysis"
	"github.com/IshaanNene/Tracepoint/internal/compare"
	"github.com/IshaanNene/Tracepoint/internal/result/resulttest"
)

// comparison is a baseline and a slower current run, with a hostile run id and
// configuration key for the escaping checks.
func comparison(t *testing.T) string {
	t.Helper()
	b := resulttest.NewRun(40).Analysed(analysis.Inputs{})
	c := resulttest.NewRun(40).Fault("http", 10, 20, 900).Analysed(analysis.Inputs{})
	c.Run.ID = "</script><img src=x onerror=alert(1)>"
	rep, err := compare.Compare(b, c, compare.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rep.ConfigDiff = append(rep.ConfigDiff, "<img src=x>")
	out, err := RenderCompare(rep, b, c)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestComparePageIsSealed(t *testing.T) {
	out := comparison(t)
	m := cspRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no policy")
	}
	policy := stdhtml.UnescapeString(m[1])
	if !strings.HasPrefix(policy, "default-src 'none'; ") {
		t.Fatalf("policy = %s", policy)
	}
	var want []string
	for _, s := range scriptRe.FindAllStringSubmatch(out, -1) {
		want = append(want, sha(s[1]))
	}
	sort.Strings(want)
	var got []string
	for _, h := range hashRe.FindAllStringSubmatch(policy, -1) {
		got = append(got, h[1])
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("an inline script is not covered by the policy")
		}
	}
	if len(want) != 3 {
		t.Fatalf("%d inline scripts, want 3", len(want))
	}
	if strings.Contains(out, "<img") {
		t.Fatal("hostile markup reached the page as markup")
	}
	if strings.Count(out, "<script") != strings.Count(out, "</script>") {
		t.Fatal("a value closed a script element early")
	}
	for _, tag := range []string{"<link", "<iframe", "<object", "<embed", "<base", `src="`, `href="http`} {
		if strings.Contains(strings.ToLower(out), tag) {
			t.Errorf("the page contains %s", tag)
		}
	}
	if !strings.Contains(out, "regression") {
		t.Fatal("the outcome is not stated")
	}
}

// In a real browser the overlay draws one chart per runner, silently.
func TestComparePageInABrowser(t *testing.T) {
	browser := chromium(t)
	path := filepath.Join(t.TempDir(), "compare.html")
	if err := os.WriteFile(path, []byte(comparison(t)), 0o600); err != nil {
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
	if n := strings.Count(stdout.String(), "<canvas"); n != 3 {
		t.Errorf("%d charts drew, want 3 (http, db, redis)", n)
	}
}
