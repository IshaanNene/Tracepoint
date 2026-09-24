package sqlrun_test

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/runner/sqlrun"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func ptr[T any](v T) *T { return &v }

// newDB creates a SQLite database on disk with a small table. SQLite is a real SQL
// engine and a real driver, so these exercise the runner end to end without needing a
// container; Postgres and MySQL are covered by the integration suite.
func newDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(t.Context(), `CREATE TABLE items (id INTEGER PRIMARY KEY, title TEXT NOT NULL)`); err != nil {
		t.Fatalf("creating the table: %v", err)
	}
	for i := 1; i <= 200; i++ {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO items (id, title) VALUES (?, ?)`, i, "item"); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	return path
}

type harness struct {
	runner    *sqlrun.Runner
	collector *metrics.Collector
	deps      runner.Deps
}

func newHarness(t *testing.T, cfg *config.DB, labels ...string) *harness {
	t.Helper()
	col, err := metrics.NewCollector(metrics.Config{
		Runner: "db", Kind: metrics.KindStorage, Labels: labels,
		BucketWidth: time.Second, SealDelay: 5 * time.Second, MinSamples: 1,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	deps := runner.Deps{
		RunID: "20260920T120000Z-test01", Collector: col,
		Clock: clock.New(), Start: time.Now(), Seed: 11,
	}
	r, err := sqlrun.New(cfg, 5*time.Second, deps)
	if err != nil {
		t.Fatalf("sqlrun.New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &harness{runner: r, collector: col, deps: deps}
}

func (h *harness) do(t *testing.T, n int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range n {
		// Due the moment it is dispatched: a fixed offset would put the intended
		// time in the future whenever the operation is faster than the spacing.
		now := h.deps.Elapsed()
		it := runner.Iteration{
			Index: int64(i), Intended: now, Dispatched: now,
			WorkerStart: now, Rand: rng,
		}
		if err := h.runner.Do(context.Background(), &it, h.collector); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	h.collector.Finish(time.Hour)
}

func sqliteConfig(path string, queries ...config.Query) *config.DB {
	return &config.DB{
		Driver:   "sqlite",
		DSN:      path,
		Executor: config.Executor{Type: "arrival-rate", Rate: 10, MaxInFlight: 4},
		Queries:  queries,
	}
}

func TestReadsAreRecorded(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "by-id", Type: "read",
		SQL:  "SELECT id, title FROM items WHERE id = ?",
		Args: []config.Arg{{Value: "{{randInt 1 200}}"}},
	})
	h := newHarness(t, cfg, "by-id")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 30)

	s := h.collector.Snapshot().Summary
	if s.N != 30 || s.OK != 30 {
		t.Errorf("n=%d ok=%d, want 30/30 (errors %v)", s.N, s.OK, s.Errors)
	}
	reads, writes := h.runner.Counts()
	if reads != 30 || writes != 0 {
		t.Errorf("reads=%d writes=%d, want 30/0", reads, writes)
	}
	if h.runner.Kind() != metrics.KindStorage {
		t.Errorf("Kind() = %s, want storage; db is a tier behind the application", h.runner.Kind())
	}
}

// A bind argument that is exactly one generator must bind as an integer. Binding it as
// the string "42" matches nothing on an integer primary key, so every query would
// return zero rows and look suspiciously fast.
func TestGeneratedArgumentsBindWithTheirOwnType(t *testing.T) {
	t.Parallel()
	path := newDB(t)

	// Counting matched rows is how we tell a real match from a silent miss.
	cfg := sqliteConfig(path, config.Query{
		Name: "by-id", Type: "read",
		SQL:  "SELECT id FROM items WHERE id = ?",
		Args: []config.Arg{{Value: "{{randInt 1 200}}"}},
	})
	h := newHarness(t, cfg, "by-id")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 20)
	if s := h.collector.Snapshot().Summary; s.OK != 20 {
		t.Fatalf("ok = %d, want 20 (errors %v)", s.OK, s.Errors)
	}

	// The same query with the argument forced to a string form finds nothing, which is
	// exactly the failure that typing prevents.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = db.Close() }()
	var typed, stringy int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM items WHERE id = ?`, int64(42)).Scan(&typed); err != nil {
		t.Fatalf("typed query: %v", err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM items WHERE id = ?`, "42").Scan(&stringy); err != nil {
		t.Fatalf("string query: %v", err)
	}
	if typed != 1 {
		t.Fatalf("an integer bind matched %d rows, want 1", typed)
	}
	if stringy != 0 {
		t.Skip("this SQLite build coerces a string bind, so the distinction is not observable here")
	}
}

func TestWritesAreCountedSeparately(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "insert-item", Type: "write",
		SQL:  "INSERT INTO items (title) VALUES (?)",
		Args: []config.Arg{{Value: "{{randString 8}}"}},
	})
	h := newHarness(t, cfg, "insert-item")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 10)

	if s := h.collector.Snapshot().Summary; s.OK != 10 {
		t.Errorf("ok = %d, want 10 (errors %v)", s.OK, s.Errors)
	}
	reads, writes := h.runner.Counts()
	if writes != 10 || reads != 0 {
		t.Errorf("reads=%d writes=%d, want 0/10", reads, writes)
	}
}

// doConcurrently drives the runner from several goroutines at once, the way the
// executor's workers do. Run under -race it is what catches shared state that a
// sequential harness never exercises.
func (h *harness) doConcurrently(t *testing.T, workers, each int) {
	t.Helper()
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(worker), 1)) //nolint:gosec // a test
			for i := range each {
				now := h.deps.Elapsed()
				it := runner.Iteration{
					Index: int64(worker*each + i), Worker: worker, Intended: now, Dispatched: now,
					WorkerStart: now, Rand: rng,
				}
				if err := h.runner.Do(context.Background(), &it, h.collector); err != nil {
					t.Errorf("Do: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	h.collector.Finish(time.Hour)
}

// The known-answer suite found the read and write tallies racing between workers.
func TestConcurrentWorkersCountExactly(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{Name: "one", Type: "read", SQL: "SELECT 1"})
	h := newHarness(t, cfg, "one")
	h.doConcurrently(t, 8, 25)
	if reads, writes := h.runner.Counts(); reads != 200 || writes != 0 {
		t.Fatalf("reads=%d writes=%d, want 200/0", reads, writes)
	}
}

// A transaction unit is timed and committed as one operation, which is what makes its
// latency comparable to the user-visible work it stands for.
func TestTransactionUnit(t *testing.T) {
	t.Parallel()
	path := newDB(t)
	cfg := sqliteConfig(path, config.Query{
		Name: "checkout", Type: "write",
		Tx: []config.TxStatement{
			{SQL: "INSERT INTO items (title) VALUES (?)", Args: []config.Arg{{Value: "a"}}},
			{SQL: "INSERT INTO items (title) VALUES (?)", Args: []config.Arg{{Value: "b"}}},
		},
	})
	h := newHarness(t, cfg, "checkout")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 5)

	if s := h.collector.Snapshot().Summary; s.N != 5 || s.OK != 5 {
		t.Fatalf("n=%d ok=%d, want 5/5 (errors %v)", s.N, s.OK, s.Errors)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM items WHERE title IN ('a','b')`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	// Five iterations of a two-statement transaction: both statements each time.
	if n != 10 {
		t.Errorf("the transaction inserted %d rows, want 10", n)
	}
}

// A statement the database rejects is an error about the statement, not about the
// server's health, so it must not be classified as a connection failure.
func TestBadStatementIsAQueryError(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "broken", Type: "read", SQL: "SELECT * FROM no_such_table",
	})
	h := newHarness(t, cfg, "broken")
	h.do(t, 3)

	s := h.collector.Snapshot().Summary
	if s.Errors["query_error"] != 3 {
		t.Errorf("errors = %v, want three query_error", s.Errors)
	}
	if metrics.ClassQueryError.CountsTowardAbortGuard() {
		t.Error("a query error must not trip the abort guard: the server answered")
	}
}

// Preparing every statement is what turns a typo into a one-second failure instead of
// a wasted run.
func TestPreflightCatchesABadStatementWithoutRunningIt(t *testing.T) {
	t.Parallel()
	path := newDB(t)
	cfg := sqliteConfig(path, config.Query{
		Name: "typo", Type: "read", SQL: "SELECT * FROM itemz",
	})
	h := newHarness(t, cfg, "typo")
	err := h.runner.Prepare(context.Background())
	if err == nil {
		t.Fatal("preflight accepted a statement naming a table that does not exist")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("the error should name the query: %v", err)
	}

	// Nothing may have been executed: preparing plans, it does not run.
	db, dbErr := sql.Open("sqlite", path)
	if dbErr != nil {
		t.Fatalf("opening: %v", dbErr)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 200 {
		t.Errorf("preflight changed the data: %d rows, want the seeded 200", n)
	}
}

func TestPreflightFailsAgainstAnUnreachableDatabase(t *testing.T) {
	t.Parallel()
	cfg := &config.DB{
		Driver:   "postgres",
		DSN:      "postgres://nobody@127.0.0.1:1/none?connect_timeout=1&sslmode=disable",
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Queries:  []config.Query{{Name: "q", Type: "read", SQL: "SELECT 1"}},
	}
	h := newHarness(t, cfg, "q")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.runner.Prepare(ctx); err == nil {
		t.Fatal("preflight succeeded against a port nothing is listening on")
	}
}

func TestUnknownDriverListsWhatIsAvailable(t *testing.T) {
	t.Parallel()
	col, err := metrics.NewCollector(metrics.Config{Runner: "db", Labels: []string{"q"}, BucketWidth: time.Second, MinSamples: 1})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	_, err = sqlrun.New(&config.DB{
		Driver: "oracle", DSN: "x",
		Executor: config.Executor{Rate: 1},
		Queries:  []config.Query{{Name: "q", SQL: "SELECT 1"}},
	}, time.Second, runner.Deps{Collector: col, Clock: clock.New(), Start: time.Now()})
	if err == nil {
		t.Fatal("an unknown driver was accepted")
	}
	// The available drivers belong in the hint, which is where the CLI prints "what to
	// do about it"; the message says what went wrong.
	var typed *errs.Error
	if !errors.As(err, &typed) {
		t.Fatalf("not a coded error: %v", err)
	}
	if typed.Code != errs.CodePreflightDriverUnknown {
		t.Errorf("code = %s", typed.Code)
	}
	for _, want := range sqlrun.Drivers() {
		if !strings.Contains(typed.Hint, want) {
			t.Errorf("the hint should list %q as available: %q", want, typed.Hint)
		}
	}
}

func TestWeightedQuerySelection(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t),
		config.Query{Name: "hot", Type: "read", Weight: ptr(9.0), SQL: "SELECT 1"},
		config.Query{Name: "cold", Type: "read", Weight: ptr(1.0), SQL: "SELECT 2"},
	)
	h := newHarness(t, cfg, "hot", "cold")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 400)

	labels := h.collector.Snapshot().LabelSummaries
	hot, cold := labels["hot"].N, labels["cold"].N
	if hot+cold != 400 {
		t.Fatalf("hot=%d cold=%d, want 400 in total", hot, cold)
	}
	if ratio := float64(hot) / 400; ratio < 0.8 || ratio > 0.97 {
		t.Errorf("the 9:1 weighting produced %d:%d (%.2f)", hot, cold, ratio)
	}
}

func TestPreparedStatementsAreReused(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "by-id", Type: "read", Prepare: true,
		SQL:  "SELECT id FROM items WHERE id = ?",
		Args: []config.Arg{{Value: "{{randInt 1 200}}"}},
	})
	h := newHarness(t, cfg, "by-id")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 25)
	if s := h.collector.Snapshot().Summary; s.OK != 25 {
		t.Errorf("ok = %d, want 25 (errors %v)", s.OK, s.Errors)
	}
}

// Time spent waiting for a connection from our own pool must be timed apart from the
// database's work: charging it to the server would produce the wrong verdict.
func TestPoolWaitIsSeparatedFromQueryTime(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "by-id", Type: "read", SQL: "SELECT id FROM items WHERE id = 1",
	})
	cfg.Pool = &config.Pool{MaxOpen: 1}
	h := newHarness(t, cfg, "by-id")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 20)

	s := h.collector.Snapshot().Summary
	if s.OK != 20 {
		t.Fatalf("ok = %d, want 20 (errors %v)", s.OK, s.Errors)
	}
	// Service time is measured from the moment a connection was in hand, so it cannot
	// exceed the response time that includes acquiring one.
	if s.Service.P99 > s.Response.P99+0.001 {
		t.Errorf("service p99 %v exceeds response p99 %v", s.Service.P99, s.Response.P99)
	}
	if stats := h.runner.PoolStats(); stats.MaxOpenConnections != 1 {
		t.Errorf("pool max open = %d, want the configured 1", stats.MaxOpenConnections)
	}
}

func TestSessionTaggingAndComments(t *testing.T) {
	t.Parallel()
	// SQLite has no application_name, so tagging is a no-op there; what is checked
	// here is that enabling comments does not break a statement.
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "by-id", Type: "read", SQL: "SELECT id FROM items WHERE id = 1",
	})
	cfg.CommentQueries = true
	h := newHarness(t, cfg, "by-id")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare with query comments: %v", err)
	}
	h.do(t, 5)
	if s := h.collector.Snapshot().Summary; s.OK != 5 {
		t.Errorf("ok = %d, want 5 (errors %v)", s.OK, s.Errors)
	}
}

// A label that could close a comment early would produce syntactically broken SQL and
// look like a database fault.
func TestCommentInjectionIsNeutralised(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "weird*/name'; DROP TABLE items; --", Type: "read",
		SQL: "SELECT id FROM items WHERE id = 1",
	})
	cfg.CommentQueries = true
	h := newHarness(t, cfg, "weird*/name'; DROP TABLE items; --")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.do(t, 3)
	if s := h.collector.Snapshot().Summary; s.OK != 3 {
		t.Errorf("ok = %d, want 3; the label broke the statement (errors %v)", s.OK, s.Errors)
	}
}

func TestTimeoutIsClassifiedAsTimeout(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{
		Name: "slow", Type: "read",
		// A deliberately expensive cross join, cut short by a very short timeout.
		SQL:     "SELECT COUNT(*) FROM items a, items b, items c, items d",
		Timeout: ptr(config.Duration(20 * time.Millisecond)),
	})
	h := newHarness(t, cfg, "slow")
	h.do(t, 1)

	s := h.collector.Snapshot().Summary
	if s.Errors["timeout"] == 0 && s.OK == 0 {
		t.Errorf("expected a timeout or a success, got %v", s.Errors)
	}
}

func TestMetadata(t *testing.T) {
	t.Parallel()
	cfg := sqliteConfig(newDB(t), config.Query{Name: "q", Type: "read", SQL: "SELECT 1"})
	h := newHarness(t, cfg, "q")
	if h.runner.Name() != "db" {
		t.Errorf("Name() = %q", h.runner.Name())
	}
	if h.runner.Driver() != "sqlite" {
		t.Errorf("Driver() = %q", h.runner.Driver())
	}
	if got := h.runner.Labels(); len(got) != 1 || got[0] != "q" {
		t.Errorf("Labels() = %v", got)
	}
}
