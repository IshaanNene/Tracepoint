package cli_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	_ "modernc.org/sqlite"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the policy: %v", err)
	}
	return path
}

// newSQLite creates a seeded database and returns its path.
func newSQLite(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE items (id INTEGER PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatalf("creating: %v", err)
	}
	for i := 1; i <= 100; i++ {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO items VALUES (?, ?)`, i, "item"); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	return path
}

// A dry run answers "what would this do" without doing any of it, which is what makes
// it safe to point at a configuration nobody has read yet.
func TestDryRunContactsNothing(t *testing.T) {
	srv := newServer(t, 0)
	dead := srv.URL
	srv.Close() // a target that would fail preflight

	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 5m, bucket: 1s, warmup: 10s }
slo:
  http: { p99: 250ms }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 200, max_in_flight: 64 }
  requests:
    - { name: probe, url: "/api/items" }
`, dead))

	r := exec(t, "run", "-c", cfg, "--dry-run", "--output", "json")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d; a dry run must not need the target to be up\nstderr: %s", r.code, r.stderr)
	}

	var plan map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &plan); err != nil {
		t.Fatalf("the plan is not JSON: %v\n%s", err, truncate(r.stdout))
	}
	if got := number(t, plan, "duration_s"); got != 300 {
		t.Errorf("duration_s = %v, want 300", got)
	}
	// 200/s held for five minutes is 60,000 operations. Knowing that before starting a
	// long test against someone else's system is the point of the plan.
	if got := number(t, plan, "total_offered_operations"); got != 60000 {
		t.Errorf("total_offered_operations = %v, want 60000", got)
	}
	runners, _ := plan["runners"].([]any)
	if len(runners) != 1 {
		t.Fatalf("got %d runners in the plan", len(runners))
	}
	rn, _ := runners[0].(map[string]any)
	if number(t, rn, "peak_rps") != 200 {
		t.Errorf("peak_rps = %v", rn["peak_rps"])
	}
	safety, _ := plan["safety"].(map[string]any)
	if safety["allow_writes"] != false {
		t.Errorf("the plan should show writes are not granted: %v", safety)
	}
}

func TestSetOverridesAreApplied(t *testing.T) {
	srv := newServer(t, 0)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 1s, bucket: 500ms, seed: 1, timeout: 2s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 5, max_in_flight: 4 }
  requests:
    - { name: probe, url: "/api/items" }
`, srv.URL))

	r := exec(t, "run", "-c", cfg, "--dry-run", "--output", "json",
		"--set", "run.duration=4s", "--set", "http.executor.rate=25")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &plan); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got := number(t, plan, "duration_s"); got != 4 {
		t.Errorf("duration_s = %v, want the overridden 4", got)
	}
	runners, _ := plan["runners"].([]any)
	rn, _ := runners[0].(map[string]any)
	if got := number(t, rn, "peak_rps"); got != 25 {
		t.Errorf("peak_rps = %v, want the overridden 25", got)
	}
	// The override reaches the recorded overrides list, so a result says how it was run.
	overrides, _ := plan["overrides"].([]any)
	if len(overrides) != 2 {
		t.Errorf("overrides = %v, want both recorded", overrides)
	}
}

// An override that quietly does nothing produces a run that looks like it tested the
// change and did not.
func TestUnknownSetPathIsRefused(t *testing.T) {
	srv := newServer(t, 0)
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 1s }
http:
  base_url: %q
  executor: { rate: 5 }
  requests: [{ name: probe, url: "/api/items" }]
`, srv.URL))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--set", "http.executor.raet=25")
	if r.code != errs.ExitUsage {
		t.Fatalf("exit %d, want %d", r.code, errs.ExitUsage)
	}
	doc := validateAgainst(t, compileSchema(t, "error"), r.stdout, "the error envelope")
	e, _ := doc["error"].(map[string]any)
	if e["code"] != "SET_UNKNOWN_PATH" {
		t.Errorf("code = %v, want SET_UNKNOWN_PATH", e["code"])
	}
	if hint, _ := e["hint"].(string); !strings.Contains(hint, "rate") {
		t.Errorf("hint = %q, want a suggestion of \"rate\"", hint)
	}
}

// Pointing a load generator at an arbitrary public host is how a test becomes an
// attack. The refusal must come before anything is contacted, and must say what a
// human would have to change.
func TestPublicTargetIsRefusedBeforeAnyLoad(t *testing.T) {
	cfg := writeConfig(t, `
version: 1
run: { duration: 5s }
http:
  executor: { rate: 5 }
  requests: [{ name: probe, url: "http://example.com/" }]
`)
	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
	if r.code != errs.ExitUsage {
		t.Fatalf("exit %d, want %d for a refused target\nstderr: %s", r.code, errs.ExitUsage, r.stderr)
	}
	doc := validateAgainst(t, compileSchema(t, "error"), r.stdout, "the error envelope")
	e, _ := doc["error"].(map[string]any)
	if e["code"] != "POLICY_TARGET_NOT_ALLOWED" {
		t.Fatalf("code = %v, want POLICY_TARGET_NOT_ALLOWED", e["code"])
	}
	hint, _ := e["hint"].(string)
	if !strings.Contains(hint, "allow_targets") {
		t.Errorf("hint = %q, want it to name the policy field a human must change", hint)
	}
}

func TestAllowlistedPublicTargetIsPermitted(t *testing.T) {
	pol := writePolicy(t, "version: 1\nallow_targets: [\"example.com\"]\n")
	cfg := writeConfig(t, `
version: 1
run: { duration: 5s }
http:
  executor: { rate: 5 }
  requests: [{ name: probe, url: "http://example.com/" }]
`)
	// A dry run proves the policy decision without generating traffic at example.com.
	r := exec(t, "run", "-c", cfg, "--policy", pol, "--dry-run", "--output", "json")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d, want 0 for an allowlisted target\nstderr: %s", r.code, r.stderr)
	}
}

// Writes need both the human's policy and the configuration to say so.
func TestWritesAreRefusedWithoutBothGrants(t *testing.T) {
	db := newSQLite(t)

	base := func(safety string) string {
		return fmt.Sprintf(`
version: 1
run: { duration: 1s, bucket: 500ms, timeout: 2s }
%s
db:
  driver: sqlite
  dsn: %q
  executor: { rate: 5, max_in_flight: 2 }
  queries:
    - { name: insert-item, sql: "INSERT INTO items (title) VALUES ('x')" }
`, safety, db)
	}

	t.Run("neither grants", func(t *testing.T) {
		r := exec(t, "run", "-c", writeConfig(t, base("")), "--output", "json", "--log-level", "error")
		if r.code != errs.ExitUsage {
			t.Fatalf("exit %d, want %d", r.code, errs.ExitUsage)
		}
		doc := validateAgainst(t, compileSchema(t, "error"), r.stdout, "the error envelope")
		e, _ := doc["error"].(map[string]any)
		if e["code"] != "POLICY_WRITES_NOT_ALLOWED" {
			t.Errorf("code = %v", e["code"])
		}
	})

	t.Run("only the configuration grants", func(t *testing.T) {
		cfg := writeConfig(t, base("safety: { allow_writes: true }"))
		r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
		if r.code != errs.ExitUsage {
			t.Fatalf("exit %d, want %d; the policy has not granted writes", r.code, errs.ExitUsage)
		}
	})

	t.Run("both grant", func(t *testing.T) {
		pol := writePolicy(t, "version: 1\nallow_writes: true\n")
		cfg := writeConfig(t, base("safety: { allow_writes: true }"))
		r := exec(t, "run", "-c", cfg, "--policy", pol, "--output", "json", "--log-level", "error")
		if r.code != errs.ExitOK {
			t.Fatalf("exit %d, want 0\nstderr: %s", r.code, r.stderr)
		}
		doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result.json")
		runners, _ := doc["runners"].([]any)
		rn, _ := runners[0].(map[string]any)
		if got := number(t, rn, "sql", "writes"); got == 0 {
			t.Error("writes were granted but none were counted")
		}
	})
}

// A destructive statement needs more than allow_writes, because TRUNCATE is not the
// same kind of thing as INSERT.
func TestDestructiveStatementNeedsItsOwnGrant(t *testing.T) {
	db := newSQLite(t)
	pol := writePolicy(t, "version: 1\nallow_writes: true\n")
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 1s, timeout: 2s }
safety: { allow_writes: true }
db:
  driver: sqlite
  dsn: %q
  executor: { rate: 5 }
  queries:
    - { name: wipe, type: write, sql: "DROP TABLE items" }
`, db))

	r := exec(t, "run", "-c", cfg, "--policy", pol, "--output", "json", "--log-level", "error")
	if r.code != errs.ExitUsage {
		t.Fatalf("exit %d, want %d", r.code, errs.ExitUsage)
	}
	doc := validateAgainst(t, compileSchema(t, "error"), r.stdout, "the error envelope")
	e, _ := doc["error"].(map[string]any)
	if e["code"] != "POLICY_DANGEROUS_NOT_ALLOWED" {
		t.Errorf("code = %v", e["code"])
	}

	// And the table must still be there: the refusal happened before anything ran.
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var n int
	if err := conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatalf("the table was dropped despite the refusal: %v", err)
	}
	if n != 100 {
		t.Errorf("%d rows, want the seeded 100", n)
	}
}

// §8: a sentinel secret must appear in no artifact. This is the end-to-end half of
// that promise, over a real run's real output.
func TestSentinelSecretAppearsInNoArtifact(t *testing.T) {
	const sentinel = "s3cr3t-must-not-leak"
	srv := newServer(t, 0)
	redisSrv := miniredis.RunT(t)
	db := newSQLite(t)
	resultPath := filepath.Join(t.TempDir(), "result.json")

	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 1s, bucket: 500ms, timeout: 2s }
http:
  base_url: %q
  headers: { Authorization: "Bearer %s" }
  executor: { rate: 5, max_in_flight: 2 }
  requests:
    - { name: probe, url: "/api/items", headers: { Cookie: "session=%s" } }
db:
  driver: sqlite
  dsn: %q
  executor: { rate: 5, max_in_flight: 2 }
  queries:
    - name: by-id
      type: read
      sql: "SELECT id FROM items WHERE id = ?"
      args: [{ value: 1, secret: true }]
redis:
  addr: %q
  password: "%s"
  executor: { rate: 5, max_in_flight: 2 }
  commands:
    - { name: get, type: read, cmd: [GET, "k"] }
`, srv.URL, sentinel, sentinel, db, redisSrv.Addr(), sentinel))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "debug", "--result-path", resultPath)
	// The Redis password is wrong for miniredis, so the run may fail to connect. What
	// matters here is only that the secret never reaches an artifact.
	for name, content := range map[string]string{
		"stdout": r.stdout,
		"stderr": r.stderr,
	} {
		if strings.Contains(content, sentinel) {
			t.Errorf("the sentinel secret leaked into %s:\n%s", name, truncate(content))
		}
	}
	if raw, err := os.ReadFile(resultPath); err == nil {
		if strings.Contains(string(raw), sentinel) {
			t.Error("the sentinel secret leaked into result.json")
		}
		// The redaction must have happened rather than the fields being absent.
		if !strings.Contains(string(raw), "[redacted]") {
			t.Error("result.json shows no redaction marker, so the secret may simply be missing rather than redacted")
		}
	}
}

func TestDoctor(t *testing.T) {
	t.Run("reports findings and passes on a workable configuration", func(t *testing.T) {
		srv := newServer(t, 0)
		cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 10s, warmup: 2s }
slo:
  http: { p99: 250ms }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 20, max_in_flight: 16 }
  requests: [{ name: probe, url: "/api/items" }]
`, srv.URL))

		r := exec(t, "doctor", "-c", cfg, "--output", "json")
		if r.code != errs.ExitOK {
			t.Fatalf("exit %d, want 0\nstdout: %s", r.code, truncate(r.stdout))
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
			t.Fatalf("the diagnosis is not JSON: %v", err)
		}
		if d["ok"] != true {
			t.Errorf("ok = %v, want true", d["ok"])
		}
		// Loopback and the absence of a storage probe are both worth saying.
		findings, _ := d["findings"].([]any)
		codes := map[string]bool{}
		for _, f := range findings {
			m, _ := f.(map[string]any)
			codes[fmt.Sprint(m["code"])] = true
		}
		if !codes["SAME_HOST_TARGET"] {
			t.Errorf("doctor did not mention that the target is on this machine: %v", codes)
		}
		if !codes["NO_STORAGE_PROBE"] {
			t.Errorf("doctor did not mention that no tier can be attributed: %v", codes)
		}
		// Every finding must carry a code, a severity and a fix, or it is a complaint
		// rather than a diagnosis.
		for _, f := range findings {
			m, _ := f.(map[string]any)
			if m["code"] == "" || m["severity"] == "" || m["message"] == "" {
				t.Errorf("incomplete finding: %v", m)
			}
		}
	})

	t.Run("fails on a target the policy refuses", func(t *testing.T) {
		cfg := writeConfig(t, `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 5 }
  requests: [{ name: probe, url: "http://example.com/" }]
`)
		r := exec(t, "doctor", "-c", cfg, "--output", "json")
		if r.code == errs.ExitOK {
			t.Fatal("doctor passed a configuration whose target the policy refuses")
		}
	})

	t.Run("warns when concurrency cannot sustain the offered rate", func(t *testing.T) {
		srv := newServer(t, 0)
		cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 10s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 1000, max_in_flight: 2 }
  requests: [{ name: probe, url: "/api/items" }]
`, srv.URL))
		r := exec(t, "doctor", "-c", cfg, "--output", "json")
		if !strings.Contains(r.stdout, "LITTLES_LAW_INCONSISTENT") {
			t.Errorf("doctor did not warn that 1000/s cannot be sustained with 2 in flight:\n%s", truncate(r.stdout))
		}
	})
}

// All three tiers on one clock is the whole point of the tool; this proves they record
// into one timeline and one result.
func TestThreeTiersShareOneTimeline(t *testing.T) {
	srv := newServer(t, 0)
	redisSrv := miniredis.RunT(t)
	db := newSQLite(t)

	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 500ms, seed: 42, timeout: 3s }
slo:
  http:  { p99: 2s }
  db:    { p99: 1s }
  redis: { p99: 1s }
http:
  base_url: %q
  executor: { type: arrival-rate, rate: 20, max_in_flight: 8 }
  requests: [{ name: list, url: "/api/items" }]
db:
  driver: sqlite
  dsn: %q
  executor: { type: arrival-rate, rate: 20, max_in_flight: 4 }
  queries:
    - { name: by-id, type: read, sql: "SELECT id FROM items WHERE id = ?", args: ["{{randInt 1 100}}"] }
redis:
  addr: %q
  executor: { type: arrival-rate, rate: 20, max_in_flight: 4 }
  commands:
    - { name: get, type: read, cmd: [GET, "session:{{randInt 1 50}}"] }
`, srv.URL, db, redisSrv.Addr()))

	r := exec(t, "run", "-c", cfg, "--output", "json", "--log-level", "error")
	if r.code != errs.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s\nstdout: %s", r.code, r.stderr, truncate(r.stdout))
	}
	doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result.json")

	runners, _ := doc["runners"].([]any)
	if len(runners) != 3 {
		t.Fatalf("got %d runners, want http, db and redis", len(runners))
	}
	byName := map[string]map[string]any{}
	for _, x := range runners {
		m, _ := x.(map[string]any)
		byName[fmt.Sprint(m["name"])] = m
	}
	for _, name := range []string{"http", "db", "redis"} {
		rn, ok := byName[name]
		if !ok {
			t.Fatalf("no %s runner in the result", name)
		}
		if n := number(t, rn, "summary", "n"); n == 0 {
			t.Errorf("%s recorded no operations", name)
		}
		// Latencies must be plausible. Measuring from the wrong origin once produced
		// figures in the billions of milliseconds, which no assertion on counts caught.
		if p99 := number(t, rn, "summary", "response_ms", "p99"); p99 <= 0 || p99 > 60_000 {
			t.Errorf("%s p99 = %vms, which is not a plausible latency", name, p99)
		}
	}
	if byName["http"]["kind"] != "app" {
		t.Errorf("http kind = %v, want app", byName["http"]["kind"])
	}
	for _, name := range []string{"db", "redis"} {
		if byName[name]["kind"] != "storage" {
			t.Errorf("%s kind = %v, want storage", name, byName[name]["kind"])
		}
	}

	// One clock means the buckets line up: every runner's timeline covers the same
	// window, which is what makes "they went slow together" observable at all.
	spans := map[string][2]float64{}
	for name, rn := range byName {
		buckets, _ := rn["buckets"].([]any)
		if len(buckets) == 0 {
			t.Fatalf("%s has no timeline", name)
		}
		first, _ := buckets[0].(map[string]any)
		last, _ := buckets[len(buckets)-1].(map[string]any)
		spans[name] = [2]float64{number(t, first, "t_ms"), number(t, last, "t_ms")}
	}
	for name, span := range spans {
		if diff := span[0] - spans["http"][0]; diff > 1000 || diff < -1000 {
			t.Errorf("%s timeline starts at %v but http starts at %v; the tiers are not on one clock",
				name, span[0], spans["http"][0])
		}
	}
}
