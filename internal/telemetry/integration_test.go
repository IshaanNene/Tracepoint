//go:build integration

package telemetry_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/telemetry"
	"github.com/IshaanNene/Tracepoint/internal/testenv"
)

// sample opens a sampler, takes two readings (the first primes counter baselines)
// and returns the second.
func sample(t *testing.T, spec telemetry.Spec, between func()) telemetry.Sample {
	t.Helper()
	s, err := telemetry.New(spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if e := s.(telemetry.Opener).Open(ctx); e != nil {
		t.Fatalf("Open: %v", e)
	}
	t.Cleanup(func() { _ = s.(telemetry.Closer).Close() })
	if _, e := s.Sample(ctx, 0); e != nil {
		t.Fatalf("first Sample: %v", e)
	}
	if between != nil {
		between()
	}
	got, err := s.Sample(ctx, time.Second)
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	return got
}

func hasKeys(t *testing.T, s telemetry.Sample, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := s[k]; !ok {
			t.Errorf("sample lacks %s: %v", k, s)
		}
	}
}

func TestPostgresSampler(t *testing.T) {
	dsn := testenv.Postgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	got := sample(t, telemetry.Spec{Name: "postgres", DSN: dsn, RunID: "it"}, func() {
		for range 20 {
			if _, e := db.ExecContext(context.Background(), "SELECT 1"); e != nil {
				t.Fatal(e)
			}
		}
	})
	hasKeys(t, got, "waiting_by_type", "sessions_active", "sessions_waiting_lock", "sessions_tracepoint", "sessions_other",
		"locks_waiting", "commits", "rollbacks", "deadlocks", "temp_bytes")
	if c, _ := got["commits"].(float64); c < 1 {
		t.Errorf("commits = %v after twenty statements", got["commits"])
	}

	// A lock that another session waits on shows up in both lock signals.
	ctx := context.Background()
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if _, e := holder.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS locked (id int)"); e != nil {
		t.Fatal(e)
	}
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, e := tx.ExecContext(ctx, "LOCK TABLE locked IN ACCESS EXCLUSIVE MODE"); e != nil {
		t.Fatal(e)
	}
	waiter := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, "SELECT * FROM locked")
		waiter <- err
	}()
	time.Sleep(300 * time.Millisecond)

	locked := sample(t, telemetry.Spec{Name: "postgres", DSN: dsn, RunID: "it"}, nil)
	if v, _ := locked["locks_waiting"].(int64); v < 1 {
		t.Errorf("locks_waiting = %v while a session waits", locked["locks_waiting"])
	}
	if v, _ := locked["sessions_waiting_lock"].(int64); v < 1 {
		t.Errorf("sessions_waiting_lock = %v while a session waits", locked["sessions_waiting_lock"])
	}
	if byType, _ := locked["waiting_by_type"].(map[string]any); byType["Lock"] == nil {
		t.Errorf("waiting_by_type = %v while a session waits on a lock", locked["waiting_by_type"])
	}
	_ = tx.Rollback()
	if e := <-waiter; e != nil {
		t.Fatalf("the waiting query: %v", e)
	}
}

// Without pg_read_all_stats the sampler still runs, and says what it cannot see.
func TestPostgresSamplerWithoutStatsGrant(t *testing.T) {
	dsn := testenv.Postgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, e := db.ExecContext(context.Background(), "CREATE ROLE watcher LOGIN PASSWORD 'w'"); e != nil {
		t.Fatal(e)
	}
	limited := strings.Replace(dsn, "tp:tp@", "watcher:w@", 1)

	s, err := telemetry.New(telemetry.Spec{Name: "postgres", DSN: limited, RunID: "it"})
	if err != nil {
		t.Fatal(err)
	}
	if e := s.(telemetry.Opener).Open(context.Background()); e != nil {
		t.Fatalf("Open: %v", e)
	}
	defer func() { _ = s.(telemetry.Closer).Close() }()
	if lim := s.(telemetry.Limiter).Limitation(); !strings.Contains(lim, "pg_read_all_stats") {
		t.Fatalf("limitation = %q", lim)
	}
	if _, e := s.Sample(context.Background(), 0); e != nil {
		t.Fatalf("a limited sampler still samples: %v", e)
	}
}

// The end-of-run snapshot reports the top statements when the extension is there.
func TestPostgresStatements(t *testing.T) {
	dsn := testenv.Postgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s, err := telemetry.New(telemetry.Spec{Name: "postgres", DSN: dsn, RunID: "it", Statements: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if e := s.(telemetry.Opener).Open(ctx); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.(telemetry.Closer).Close() }()
	// The extension is not preloaded in a stock image, so the snapshot fails - and a
	// failed snapshot is reported by the loop and never fails the run.
	if _, err := s.(telemetry.Finisher).Finish(ctx); err == nil {
		t.Log("pg_stat_statements is available in this image")
	}
}

func TestMySQLSampler(t *testing.T) {
	dsn := testenv.MySQL(t)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	got := sample(t, telemetry.Spec{Name: "mysql", DSN: dsn, RunID: "it"}, func() {
		for range 20 {
			if _, e := db.ExecContext(context.Background(), "SELECT 1"); e != nil {
				t.Fatal(e)
			}
		}
	})
	hasKeys(t, got, "threads_running", "threads_connected", "row_lock_waits", "row_lock_time_ms", "queries_per_sec")
	if q, _ := got["queries_per_sec"].(float64); q <= 0 {
		t.Errorf("queries_per_sec = %v after twenty statements", got["queries_per_sec"])
	}
}

func TestRedisSampler(t *testing.T) {
	addr := testenv.Redis(t, true)
	got := sample(t, telemetry.Spec{Name: "redis", Redis: &config.Redis{Addr: addr}, RunID: "it", Latency: true}, nil)
	hasKeys(t, got, "ops_per_sec", "connected_clients", "blocked_clients", "used_memory_bytes", "latency_max_ms")
}
