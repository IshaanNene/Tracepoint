package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// quick runs a real smoke test with both probes: the stored configuration references
// the DSN given on the command line rather than holding it, and the digest says what
// a SELECT 1 probe cannot see. (A password in a DSN is redacted from every artifact;
// the secret-redaction suite covers that.)
func TestQuickProbesEveryTier(t *testing.T) {
	srv := newServer(t, 0)
	rd := miniredis.RunT(t)
	dsn := filepath.Join(t.TempDir(), "quick-sentinel.db")
	root := runRoot(t)

	r := exec(t, "quick", srv.URL+"/api/items", "--db-dsn", dsn, "--redis", rd.Addr(),
		"--rate", "20", "--duration", "2s", "--output", "json", "--log-level", "error")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result.json")
	var names []string
	for _, rn := range doc["runners"].([]any) {
		names = append(names, rn.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "http,db,redis" {
		t.Fatalf("runners %v", names)
	}

	var id string
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			id = e.Name()
		}
	}
	stored, _ := os.ReadFile(filepath.Join(root, id, "config.effective.yaml"))
	if bytes.Contains(stored, []byte("quick-sentinel")) || !bytes.Contains(stored, []byte("${TRACEPOINT_QUICK_DB_DSN}")) {
		t.Errorf("the stored configuration holds the DSN instead of referencing it:\n%s", stored)
	}
	d := exec(t, "digest", id)
	digest := validateAgainst(t, compileSchema(t, "digest"), d.stdout, "digest")
	caveats, _ := digest["caveats"].([]any)
	found := false
	for _, c := range caveats {
		found = found || c.(map[string]any)["code"] == "PROBE_TRIVIAL"
	}
	if !found {
		t.Fatalf("the digest does not state the SELECT 1 blind spot: %v", caveats)
	}
}

func TestQuickPrintsAndRefuses(t *testing.T) {
	r := exec(t, "quick", "http://127.0.0.1:9/x?a=1", "--db-dsn", "postgres://u:hunter2@db/app", "--print-config")
	if r.code != 0 || strings.Contains(r.stdout, "hunter2") || !strings.Contains(r.stdout, "${TRACEPOINT_QUICK_DB_DSN}") ||
		!strings.Contains(r.stdout, `url: "/x?a=1"`) || !strings.Contains(r.stdout, "driver: postgres") {
		t.Fatalf("print-config:\n%s", r.stdout)
	}
	for name, args := range map[string][]string{
		"not a url":      {"quick", "127.0.0.1:8080"},
		"unknown driver": {"quick", "http://127.0.0.1:9/", "--db-dsn", "whatever"},
		"two dsns":       {"quick", "http://127.0.0.1:9/", "--db-dsn", "a.db", "--db-dsn-env", "X"},
	} {
		if r := exec(t, args...); r.code != errs.ExitUsage {
			t.Errorf("%s: exit %d", name, r.code)
		}
	}
}
