package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// A run writes its result; digest reads it back and emits one schema-valid document.
func TestDigestOfARun(t *testing.T) {
	srv := newServer(t, 0)
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 500ms, seed: 3, timeout: 3s }
http:
  base_url: %q
  executor: { rate: 40, max_in_flight: 16 }
  requests:
    - { name: list-items, url: "/api/items" }
`, srv.URL))
	if r := exec(t, "run", "-c", cfg, "--result-path", resultPath, "--log-level", "error"); r.code != errs.ExitOK {
		t.Fatalf("run exited %d: %s", r.code, r.stderr)
	}

	// Both a result path and the directory holding it name the run.
	for _, arg := range []string{resultPath, dir} {
		r := exec(t, "digest", arg)
		if r.code != errs.ExitOK {
			t.Fatalf("digest %s exited %d: %s", arg, r.code, r.stderr)
		}
		doc := validateAgainst(t, compileSchema(t, "digest"), r.stdout, "digest")
		if doc["run_id"] == "" || doc["truncated"] != false {
			t.Fatalf("digest = %s", truncate(r.stdout))
		}
		verdict, _ := doc["verdict"].(map[string]any)
		if verdict["caveat"] != "Shared-clock correlation shows association, not causation." {
			t.Fatalf("verdict = %v", verdict)
		}
	}

	// The budget is honoured, and a truncated digest says how to get the rest.
	r := exec(t, "digest", resultPath, "--budget-chars", "900")
	var d map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if d["truncated"] != true {
		t.Fatalf("a 900-character budget should truncate: %s", r.stdout)
	}
	more, _ := d["more"].(map[string]any)
	if hint, _ := more["hint"].(string); !strings.Contains(hint, "--budget-chars") {
		t.Fatalf("more = %v", more)
	}
}

func TestDigestOfNothing(t *testing.T) {
	r := exec(t, "digest", filepath.Join(t.TempDir(), "absent.json"), "--output", "json")
	if r.code != errs.ExitUsage {
		t.Fatalf("exit %d, want %d", r.code, errs.ExitUsage)
	}
	validateAgainst(t, compileSchema(t, "error"), r.stdout, "error envelope")
	if !strings.Contains(r.stdout, "RESULT_NOT_FOUND") {
		t.Fatalf("stdout = %s", r.stdout)
	}

	empty := t.TempDir()
	if r := exec(t, "digest", empty); r.code != errs.ExitUsage || !strings.Contains(r.stderr, "holds no result.json") {
		t.Fatalf("an empty run directory: exit %d, %s", r.code, r.stderr)
	}

	notResult := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(notResult, []byte(`{"hello":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := exec(t, "digest", notResult); r.code != errs.ExitUsage || !strings.Contains(r.stderr, "RESULT_PARSE") {
		t.Fatalf("a non-result: exit %d, %s", r.code, r.stderr)
	}
}
