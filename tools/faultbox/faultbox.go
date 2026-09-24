// Package faultbox is a small application over Postgres and Redis whose faults can be
// injected at known times. It exists to prove TracePoint's verdicts end to end (§10):
// a methodology that cannot name the cause of a fault it was told about in advance is
// not a methodology.
//
// The application serves GET /api/items/{id}, which reads the item from Postgres and
// its cache entry from Redis - so a database stall and a cache stall both reach users.
// A separate admin handler schedules faults:
//
//	POST   /admin/faults  {"kind": "pg_lock", "table": "items", "at": "20s", "duration": "5s"}
//	GET    /admin/faults  every fault with when it actually began and ended
//	DELETE /admin/faults  cancel pending faults and clear any handler delay
//
// Kinds:
//
//	pg_lock      hold ACCESS EXCLUSIVE on a table (items, which the endpoint and a probe
//	             both read, or probe_only, which only a probe reads) for duration
//	redis_sleep  DEBUG SLEEP for duration; needs a server started with
//	             --enable-debug-command yes
//	delay        add delay to every request in the handler, for duration or, with no
//	             duration, until cleared
//
// "at" is an offset from the start of the run: the first request that carries a
// TracePoint run id and is not a preflight. That is what lets a test put a fault at a
// known bucket without seeing the run's clock. Without "at" a fault begins at once.
package faultbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // the Postgres driver
	"github.com/redis/go-redis/v9"
)

// Headers faultbox reads. They match what TracePoint sends.
const (
	runIDHeader     = "X-Tracepoint-Run-Id"
	preflightHeader = "X-Tracepoint-Preflight"
)

// Items is how many rows each table is seeded with.
const Items = 1000

// Config is how faultbox connects.
type Config struct {
	DSN       string
	RedisAddr string
	// PoolSize bounds the application's own Postgres and Redis pools.
	PoolSize int
	Logger   *slog.Logger
}

// App is a running faultbox.
type App struct {
	cfg   Config
	db    *sql.DB
	rdb   *redis.Client
	log   *slog.Logger
	delay atomic.Int64 // nanoseconds added to every request

	mu       sync.Mutex
	runID    string
	runStart time.Time
	started  chan struct{} // closed when the current run's first request arrives
	faults   []*Fault
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Fault is one scheduled or completed fault.
type Fault struct {
	ID       int      `json:"id"`
	Kind     string   `json:"kind"`
	Table    string   `json:"table,omitempty"`
	At       Duration `json:"at,omitempty"`
	Duration Duration `json:"duration,omitempty"`
	Delay    Duration `json:"delay,omitempty"`
	// BeganMS and EndedMS are offsets from the run start at which the fault actually
	// took hold and let go, so a test can assert against what happened rather than
	// what was asked for.
	BeganMS *float64 `json:"began_ms,omitempty"`
	EndedMS *float64 `json:"ended_ms,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Duration reads "250ms"-style strings in JSON.
type Duration time.Duration

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration must be a string such as \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes a duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// New connects faultbox. Call Setup to create and seed its tables.
func New(ctx context.Context, cfg Config) (*App, error) {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 100
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	db.SetMaxOpenConns(cfg.PoolSize)
	db.SetMaxIdleConns(cfg.PoolSize)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, PoolSize: cfg.PoolSize})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = db.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}
	actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &App{
		cfg: cfg, db: db, rdb: rdb, log: cfg.Logger,
		started: make(chan struct{}), ctx: actx, cancel: cancel,
	}, nil
}

// Setup creates the tables and seeds them and the cache.
func (a *App) Setup(ctx context.Context) error {
	for _, table := range []string{"items", "probe_only"} {
		stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id int PRIMARY KEY, title text NOT NULL)`, table)
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("creating %s: %w", table, err)
		}
		seed := fmt.Sprintf(`INSERT INTO %s (id, title)
			SELECT g, 'item ' || g FROM generate_series(1, %d) g ON CONFLICT (id) DO NOTHING`, table, Items)
		if _, err := a.db.ExecContext(ctx, seed); err != nil {
			return fmt.Errorf("seeding %s: %w", table, err)
		}
	}
	pipe := a.rdb.Pipeline()
	for i := 1; i <= Items; i++ {
		pipe.Set(ctx, "item:"+strconv.Itoa(i), "item "+strconv.Itoa(i), 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("seeding redis: %w", err)
	}
	return nil
}

// Close cancels pending faults and disconnects.
func (a *App) Close() error {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	cancel()
	a.wg.Wait()
	return errors.Join(a.db.Close(), a.rdb.Close())
}

// Handler serves the application.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items/{id}", a.item)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

// AdminHandler serves fault injection. Serve it on a separate listener: it is not part
// of the application under test.
func (a *App) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/faults", a.schedule)
	mux.HandleFunc("GET /admin/faults", a.list)
	mux.HandleFunc("DELETE /admin/faults", a.clear)
	return mux
}

func (a *App) item(w http.ResponseWriter, r *http.Request) {
	a.noteRun(r)
	if d := time.Duration(a.delay.Load()); d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var title string
	err = a.db.QueryRowContext(r.Context(), "SELECT title FROM items WHERE id = $1", id).Scan(&title)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	cached, err := a.rdb.Get(r.Context(), "item:"+strconv.Itoa(id)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		http.Error(w, "cache error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, a.log, map[string]any{"id": id, "title": title, "cached": cached != ""})
}

// noteRun starts the run clock at the first load request of a new run.
func (a *App) noteRun(r *http.Request) {
	id := r.Header.Get(runIDHeader)
	if id == "" || r.Header.Get(preflightHeader) != "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id == a.runID {
		return
	}
	a.runID, a.runStart = id, time.Now()
	close(a.started)
	a.log.Info("run started", "run_id", id)
}

// sinceStart is the offset from the current run's start.
func (a *App) sinceStart() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runStart.IsZero() {
		return 0
	}
	return float64(time.Since(a.runStart)) / float64(time.Millisecond)
}

// schedule accepts a fault. The fault runs on the app's own context, not the request's:
// it has to outlive the admin call that scheduled it.
//
//nolint:contextcheck // deliberately detached from the request, see above
func (a *App) schedule(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch f.Kind {
	case "pg_lock":
		if f.Table != "items" && f.Table != "probe_only" {
			http.Error(w, "table must be items or probe_only", http.StatusBadRequest)
			return
		}
		if f.Duration <= 0 {
			http.Error(w, "pg_lock needs a duration", http.StatusBadRequest)
			return
		}
	case "redis_sleep":
		if f.Duration <= 0 {
			http.Error(w, "redis_sleep needs a duration", http.StatusBadRequest)
			return
		}
	case "delay":
		if f.Delay <= 0 {
			http.Error(w, "delay needs a delay", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "kind must be pg_lock, redis_sleep or delay", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	f.ID = len(a.faults) + 1
	fault := &f
	a.faults = append(a.faults, fault)
	started, ctx := a.started, a.ctx
	reply := *fault // the goroutine below mutates fault; reply with what was accepted
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.run(ctx, fault, started)
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, a.log, reply)
}

// run waits for the run to start and the fault's offset to arrive, then applies it.
func (a *App) run(ctx context.Context, f *Fault, started chan struct{}) {
	if f.At > 0 {
		select {
		case <-started:
		case <-ctx.Done():
			return
		}
		a.mu.Lock()
		due := a.runStart.Add(time.Duration(f.At))
		a.mu.Unlock()
		select {
		case <-time.After(time.Until(due)):
		case <-ctx.Done():
			return
		}
	}

	var err error
	switch f.Kind {
	case "pg_lock":
		err = a.lock(ctx, f)
	case "redis_sleep":
		err = a.sleep(ctx, f)
	case "delay":
		a.mark(f, true)
		a.delay.Store(int64(f.Delay))
		if f.Duration > 0 {
			select {
			case <-time.After(time.Duration(f.Duration)):
			case <-ctx.Done():
			}
			a.delay.Store(0)
			a.mark(f, false)
		}
	}
	if err != nil {
		a.mu.Lock()
		f.Error = err.Error()
		a.mu.Unlock()
		a.log.Error("fault failed", "kind", f.Kind, "err", err)
	}
}

func (a *App) mark(f *Fault, began bool) {
	at := a.sinceStart()
	a.mu.Lock()
	defer a.mu.Unlock()
	if began {
		f.BeganMS = &at
	} else {
		f.EndedMS = &at
	}
}

// lock holds ACCESS EXCLUSIVE on a table inside a transaction, then rolls back. Every
// reader of that table queues behind it for the duration.
func (a *App) lock(ctx context.Context, f *Fault) error {
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			a.log.Debug("rolling back the lock transaction", "err", rbErr)
		}
	}()
	// The table is one of two fixed names, checked when the fault was scheduled.
	if _, lockErr := tx.ExecContext(ctx, "LOCK TABLE "+f.Table+" IN ACCESS EXCLUSIVE MODE"); lockErr != nil {
		return fmt.Errorf("locking %s: %w", f.Table, lockErr)
	}
	a.mark(f, true)
	select {
	case <-time.After(time.Duration(f.Duration)):
	case <-ctx.Done():
	}
	err = tx.Rollback()
	a.mark(f, false)
	if err != nil {
		return fmt.Errorf("releasing the lock: %w", err)
	}
	return nil
}

// sleep freezes the Redis server with DEBUG SLEEP. It uses its own connection, since
// the command blocks the server and therefore the connection that sent it.
func (a *App) sleep(ctx context.Context, f *Fault) error {
	c := redis.NewClient(&redis.Options{Addr: a.cfg.RedisAddr, PoolSize: 1, ReadTimeout: -1})
	defer func() { _ = c.Close() }()
	a.mark(f, true)
	secs := strconv.FormatFloat(time.Duration(f.Duration).Seconds(), 'f', 3, 64)
	err := c.Do(ctx, "DEBUG", "SLEEP", secs).Err()
	a.mark(f, false)
	if err != nil {
		return fmt.Errorf("DEBUG SLEEP (is the server started with --enable-debug-command yes?): %w", err)
	}
	return nil
}

func (a *App) list(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	out := make([]Fault, 0, len(a.faults))
	for _, f := range a.faults {
		out = append(out, *f)
	}
	writeJSON(w, a.log, map[string]any{"run_id": a.runID, "faults": out})
}

// clear cancels pending faults, lifts any delay and forgets the run, so the next one
// starts a fresh clock.
func (a *App) clear(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	cancel := a.cancel
	a.ctx, a.cancel = context.WithCancel(context.WithoutCancel(r.Context()))
	a.faults = nil
	a.runID, a.runStart = "", time.Time{}
	a.started = make(chan struct{})
	a.mu.Unlock()

	cancel()
	a.wg.Wait()
	a.delay.Store(0)
	w.WriteHeader(http.StatusNoContent)
}

// writeJSON writes a response. A client that has gone away is not faultbox's problem,
// so a failed write is logged, not returned.
func writeJSON(w http.ResponseWriter, log *slog.Logger, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Debug("writing a response", "err", err)
	}
}
