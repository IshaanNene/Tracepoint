//go:build integration

package cli_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
	"github.com/IshaanNene/Tracepoint/internal/testenv"
)

// runThreeTiers runs HTTP, one SQL dialect and Redis with every sampler on, and
// returns the result.
func runThreeTiers(t *testing.T, driver, dsn, sampler, table string) *result.Result {
	t.Helper()
	srv := newServer(t, 0)
	redisAddr := testenv.Redis(t, false)
	resultPath := filepath.Join(t.TempDir(), "result.json")

	placeholder := "$1"
	if driver == "mysql" {
		placeholder = "?"
	}
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 4s, bucket: 1s, timeout: 5s, seed: 9 }
http:
  base_url: %q
  executor: { rate: 30, max_in_flight: 16 }
  requests: [{ name: items, url: "/api/items" }]
db:
  driver: %s
  dsn: %q
  executor: { rate: 30, max_in_flight: 16 }
  queries:
    - { name: by-id, type: read, sql: "SELECT id FROM %s WHERE id = %s", args: ["{{randInt 1 3}}"] }
redis:
  addr: %q
  executor: { rate: 30, max_in_flight: 16 }
  commands: [{ name: get, type: read, cmd: [GET, "k:{{randInt 1 9}}"] }]
telemetry: { %s: true, redis: true, interval: 500ms }
`, srv.URL, driver, dsn, table, placeholder, redisAddr, sampler))

	r := exec(t, "run", "-c", cfg, "--result-path", resultPath, "--log-level", "warn")
	if r.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, schemas.Result, raw)
	res, err := result.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, rn := range res.Runners {
		if rn.Summary.N == 0 || rn.Summary.ErrorsTotal > 0 {
			t.Errorf("%s: n=%d errors=%v", rn.Name, rn.Summary.N, rn.Summary.Errors)
		}
	}
	d := exec(t, "digest", resultPath)
	schematest.Validate(t, schemas.Digest, []byte(d.stdout))
	return res
}

func samplerOK(t *testing.T, name string, s *result.SamplerSeries) {
	t.Helper()
	if s == nil || !s.Available || len(s.Samples) < 3 {
		b, _ := json.Marshal(s)
		t.Errorf("%s telemetry = %s", name, b)
	}
}

func TestThreeTiersAgainstPostgres(t *testing.T) {
	dsn := testenv.Postgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, e := db.ExecContext(context.Background(), "CREATE TABLE items (id int PRIMARY KEY); INSERT INTO items VALUES (1),(2),(3)"); e != nil {
		t.Fatal(e)
	}

	res := runThreeTiers(t, "postgres", dsn, "postgres", "items")
	samplerOK(t, "postgres", res.Telemetry.Postgres)
	samplerOK(t, "redis", res.Telemetry.Redis)
	if res.Telemetry.Generator == nil || len(res.Telemetry.Generator.Samples) < 3 {
		t.Errorf("generator telemetry missing")
	}
	// The probe tagged its sessions, so the sampler could tell them from anyone else's.
	seen := false
	for _, s := range res.Telemetry.Postgres.Samples {
		if v, _ := s["sessions_tracepoint"].(float64); v > 0 {
			seen = true
		}
	}
	if !seen {
		t.Errorf("the sampler never saw a session tagged tracepoint/<run>: %v", res.Telemetry.Postgres.Samples)
	}
}

func TestThreeTiersAgainstMySQL(t *testing.T) {
	dsn := testenv.MySQL(t)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{"CREATE TABLE items (id int PRIMARY KEY)", "INSERT INTO items VALUES (1),(2),(3)"} {
		if _, e := db.ExecContext(context.Background(), stmt); e != nil {
			t.Fatal(e)
		}
	}
	res := runThreeTiers(t, "mysql", dsn, "mysql", "items")
	samplerOK(t, "mysql", res.Telemetry.MySQL)
}

// A sampler that cannot connect degrades the run with a reason; it never fails it.
func TestUnreachableSamplerDegradesTheRun(t *testing.T) {
	srv := newServer(t, 0)
	redisAddr := testenv.Redis(t, false)
	resultPath := filepath.Join(t.TempDir(), "result.json")
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
run: { duration: 2s, bucket: 1s, timeout: 3s }
http:
  base_url: %q
  executor: { rate: 20, max_in_flight: 8 }
  requests: [{ name: items, url: "/api/items" }]
telemetry:
  redis: { dsn: %q }
  postgres: { dsn: "postgres://nobody:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=2" }
`, srv.URL, redisAddr))
	r := exec(t, "run", "-c", cfg, "--result-path", resultPath, "--output", "json", "--log-level", "error")
	if r.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	doc := validateAgainst(t, compileSchema(t, "result"), r.stdout, "result")
	tel, _ := doc["telemetry"].(map[string]any)
	pg, _ := tel["postgres"].(map[string]any)
	if pg["available"] != false || pg["reason"] == "" {
		t.Fatalf("postgres telemetry = %v", pg)
	}
	found := false
	analysis, _ := doc["analysis"].(map[string]any)
	validity, _ := analysis["validity"].(map[string]any)
	findings, _ := validity["findings"].([]any)
	for _, f := range findings {
		if m, _ := f.(map[string]any); m["code"] == "TELEMETRY_UNAVAILABLE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no TELEMETRY_UNAVAILABLE finding: %v", findings)
	}
}
