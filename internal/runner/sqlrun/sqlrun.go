// Package sqlrun drives SQL load and, at the same time, probes the database.
//
// A storage runner is both things at once. It queries the database directly, so its
// latency is a measurement of that tier's health while the application is under
// pressure - which is what makes "the application slowed down and so did the database
// probe" an observation rather than a guess.
//
// Two details matter more than they look. Connections are acquired explicitly with
// db.Conn so that time spent waiting for the pool is timed apart from time spent
// waiting for the database; charging our own pool exhaustion to the server would
// produce exactly the wrong verdict. And every row of every read is iterated and
// closed, because a query whose rows are never read has not measured the work of
// producing them.
package sqlrun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// Drivers, registered by importing them. All three are pure Go, which is what lets
	// release builds set CGO_ENABLED=0 and still ship SQLite (ADR-007).
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/template"
)

// Name is how this runner is registered and how it appears in result.json.
const Name = "db"

// drivers maps a configured driver name to the database/sql driver that serves it.
//
//nolint:gochecknoglobals // A fixed table, never mutated after init.
var drivers = map[string]string{
	"postgres": "pgx",
	"mysql":    "mysql",
	"sqlite":   "sqlite",
}

// Drivers lists what this build supports, sorted, for error messages and the
// capabilities manifest.
func Drivers() []string { return []string{"mysql", "postgres", "sqlite"} }

// Runner drives SQL load.
type Runner struct {
	cfg    *config.DB
	deps   runner.Deps
	db     *sql.DB
	driver string

	queries []*query
	picker  *runner.Weighted
	shared  *template.Shared
	timeout time.Duration

	reads, writes int64
}

// query is one compiled, weighted unit of work: a single statement or a transaction.
type query struct {
	name    string
	label   metrics.LabelID
	op      policy.Op
	isTx    bool
	stmts   []*statement
	timeout time.Duration
	prepare bool
}

type statement struct {
	sql *template.Template
	// text is the statement as it will be sent when it needs no rendering, already
	// carrying its tracing comment.
	text    string
	args    []*template.Template
	prepped *sql.Stmt
}

// New builds a SQL runner from an already-validated configuration.
func New(cfg *config.DB, defaultTimeout time.Duration, deps runner.Deps) (*Runner, error) {
	if cfg == nil {
		return nil, errs.New(errs.CodeInternal, "the db runner needs a configuration")
	}
	driverName := config.NormaliseDriver(cfg.Driver)
	sqlDriver, ok := drivers[driverName]
	if !ok {
		return nil, errs.New(errs.CodePreflightDriverUnknown,
			"no driver named %q is compiled into this build", cfg.Driver).
			WithPath("/db/driver").
			WithHint("available drivers: %s", strings.Join(Drivers(), ", "))
	}

	r := &Runner{
		cfg: cfg, deps: deps, driver: driverName,
		shared: template.NewShared(deps.Clock), timeout: defaultTimeout,
	}

	weights := make([]float64, 0, len(cfg.Queries))
	for i := range cfg.Queries {
		q, err := r.compile(&cfg.Queries[i])
		if err != nil {
			return nil, err
		}
		r.queries = append(r.queries, q)
		weights = append(weights, cfg.Queries[i].WeightOr())
	}
	r.picker = runner.NewWeighted(weights)

	dsn, err := r.tagDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalidValue, err, "opening the %s connection", driverName).
			WithPath("/db/dsn").
			WithHint("check the connection string; its password is never logged")
	}
	r.configurePool(db)
	r.db = db
	return r, nil
}

func (r *Runner) compile(src *config.Query) (*query, error) {
	q := &query{
		name:    src.Name,
		label:   r.deps.Collector.LabelID(src.Name),
		timeout: r.timeout,
		prepare: src.Prepare,
		isTx:    len(src.Tx) > 0,
	}
	if src.Timeout != nil {
		q.timeout = src.Timeout.D()
	}

	raw := make([]struct {
		sql  string
		args []config.Arg
	}, 0, len(src.Tx)+1)
	if src.SQL != "" {
		raw = append(raw, struct {
			sql  string
			args []config.Arg
		}{src.SQL, src.Args})
	}
	for _, tx := range src.Tx {
		raw = append(raw, struct {
			sql  string
			args []config.Arg
		}{tx.SQL, tx.Args})
	}

	// The declared type wins where it is given: the author knows what the statement
	// does and the classifier is inferring. Where it is absent, anything unrecognised
	// counts as a write.
	q.op = policy.Op(src.Type)
	inferred := policy.OpRead
	for _, item := range raw {
		if policy.ClassifySQL(item.sql).Op == policy.OpWrite {
			inferred = policy.OpWrite
		}
	}
	if q.op == "" {
		q.op = inferred
	}

	for _, item := range raw {
		st := &statement{}
		tpl, err := template.Compile(item.sql)
		if err != nil {
			return nil, wrapTemplate(err, src.Name, "sql")
		}
		st.sql = tpl
		if tpl.IsStatic() {
			st.text = r.comment(src.Name) + tpl.Static()
		}
		for i := range item.args {
			argTpl, err := compileArg(item.args[i])
			if err != nil {
				return nil, wrapTemplate(err, src.Name, fmt.Sprintf("argument %d", i))
			}
			st.args = append(st.args, argTpl)
		}
		q.stmts = append(q.stmts, st)
	}
	return q, nil
}

// compileArg turns a bind argument into a template. A non-string argument - a number
// written directly in the configuration - is already a value and is wrapped so the
// render path is uniform.
func compileArg(a config.Arg) (*template.Template, error) {
	s, ok := a.Value.(string)
	if !ok {
		return template.Literal(a.Value), nil
	}
	return template.Compile(s)
}

func wrapTemplate(err error, query, what string) error {
	var typed *errs.Error
	if errors.As(err, &typed) {
		return typed.WithPath("/db/queries").
			WithDetail("query", query).
			WithDetail("in", what)
	}
	return err
}

// comment prefixes a statement with a sqlcommenter-style marker, so a DBA looking at
// pg_stat_activity, pg_stat_statements or a slow log can tell which statements came
// from a load test and which came from the application.
func (r *Runner) comment(label string) string {
	if !r.cfg.CommentQueries {
		return ""
	}
	return fmt.Sprintf("/* tracepoint_run='%s',label='%s' */ ", sanitiseComment(r.deps.RunID), sanitiseComment(label))
}

// sanitiseComment removes anything that could close the comment early. A label is
// author-controlled rather than target-controlled, but a stray */ would produce
// syntactically broken SQL that looked like a database fault.
func sanitiseComment(s string) string {
	s = strings.ReplaceAll(s, "*/", "")
	s = strings.ReplaceAll(s, "'", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// tagDSN adds application_name so the database itself can attribute the traffic.
// Postgres surfaces it in pg_stat_activity, which is what lets the telemetry sampler
// separate TracePoint's own sessions from the application's.
func (r *Runner) tagDSN(dsn string) (string, error) {
	tag := "tracepoint/" + r.deps.RunID
	if r.cfg.TagSessions != nil && !*r.cfg.TagSessions {
		return dsn, nil
	}
	if r.driver != "postgres" {
		return dsn, nil
	}
	if strings.Contains(dsn, "application_name") {
		return dsn, nil // the operator set their own; leave it alone
	}

	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errs.Wrap(errs.CodeConfigInvalidValue, err, "the db dsn is not a valid url").
				WithPath("/db/dsn")
		}
		q := u.Query()
		q.Set("application_name", tag)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return dsn + " application_name='" + tag + "'", nil
}

// configurePool sizes the pool. The default is the worker count, because a pool
// smaller than the concurrency turns every operation into a queue and reports the wait
// as database latency.
func (r *Runner) configurePool(db *sql.DB) {
	workers := r.cfg.Executor.MaxInFlight
	if workers <= 0 {
		workers = 16
	}
	maxOpen, maxIdle := workers, workers
	if p := r.cfg.Pool; p != nil {
		if p.MaxOpen > 0 {
			maxOpen = p.MaxOpen
		}
		if p.MaxIdle > 0 {
			maxIdle = p.MaxIdle
		} else {
			maxIdle = maxOpen
		}
		if p.ConnMaxLifetime != nil {
			db.SetConnMaxLifetime(p.ConnMaxLifetime.D())
		}
		if p.ConnMaxIdleTime != nil {
			db.SetConnMaxIdleTime(p.ConnMaxIdleTime.D())
		}
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
}

// SetStart fixes the run's monotonic origin. The engine calls it once, after preflight
// and before any load, because every offset this runner records is measured from it.
func (r *Runner) SetStart(t time.Time) { r.deps.Start = t }

// Name identifies the runner.
func (r *Runner) Name() string { return Name }

// Kind reports that this is a tier behind the application.
func (r *Runner) Kind() metrics.Kind { return metrics.KindStorage }

// Labels are the query names.
func (r *Runner) Labels() []string {
	out := make([]string, 0, len(r.queries))
	for _, q := range r.queries {
		out = append(out, q.name)
	}
	return out
}

// Driver is the normalised driver name, for the result document.
func (r *Runner) Driver() string { return r.driver }

// Counts reports how many operations were reads and how many were writes.
func (r *Runner) Counts() (reads, writes int64) { return r.reads, r.writes }

// PoolStats reports what the connection pool did. Waits here are generator-side and
// must not be read as database latency.
func (r *Runner) PoolStats() sql.DBStats { return r.db.Stats() }

// Prepare connects, checks the server, and prepares every statement.
//
// Preparing is the valuable part: it asks the database to parse and plan each
// statement without executing it, so a typo, a missing column or a wrong table name is
// reported in a second rather than after a run has produced nothing but errors. It
// costs one round trip per statement and changes no data.
func (r *Runner) Prepare(ctx context.Context) error {
	if err := r.db.PingContext(ctx); err != nil {
		return errs.Wrap(errs.CodePreflightDBPing, err, "the %s database did not answer", r.driver).
			WithHint("check the dsn, that the server is running, and that this host may connect to it")
	}

	for _, q := range r.queries {
		for i, st := range q.stmts {
			text, err := r.staticText(st, q.name)
			if err != nil {
				// A statement built from a template cannot be prepared ahead of time;
				// its shape is still checked by compiling the template at load.
				continue
			}
			prepared, err := r.db.PrepareContext(ctx, text)
			if err != nil {
				return errs.Wrap(errs.CodePreflightDBPrepare, err,
					"statement %d of query %q could not be prepared", i, q.name).
					WithPath("/db/queries").
					WithDetail("query", q.name).
					WithHint("the database rejected the statement before it was run; check table and column names")
			}
			if q.prepare {
				st.prepped = prepared
				continue
			}
			if err := prepared.Close(); err != nil {
				r.deps.Logger.Debug("closing a preflight statement", "query", q.name, "err", err)
			}
		}
	}
	return nil
}

// staticText returns a statement's text when it needs no rendering.
func (r *Runner) staticText(st *statement, label string) (string, error) {
	if st.text != "" {
		return st.text, nil
	}
	if st.sql.IsStatic() {
		return r.comment(label) + st.sql.Static(), nil
	}
	return "", errs.New(errs.CodeInternal, "the statement is templated")
}

// Do performs one query or transaction and records its outcome.
func (r *Runner) Do(ctx context.Context, it *runner.Iteration, rec metrics.Recorder) error {
	q := r.queries[r.picker.Pick(it.Rand)]

	var o metrics.Outcome
	it.StampOutcome(&o)
	o.Label = q.label

	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()

	tctx := &template.Context{Shared: r.shared, Rand: it.Rand}

	// Acquire the connection explicitly. Everything before this line is our own
	// queueing; everything after it is the database's work. Collapsing the two would
	// make an undersized pool indistinguishable from a slow server.
	conn, err := r.db.Conn(ctx)
	if err != nil {
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		o.Class = poolClass(ctx, err)
		rec.Record(&o)
		return nil
	}
	o.ConnAcquired = r.deps.Elapsed()

	execErr := r.execute(ctx, conn, q, tctx)
	if closeErr := conn.Close(); closeErr != nil {
		r.deps.Logger.Debug("returning a connection to the pool", "err", closeErr)
	}
	o.End = r.deps.Elapsed()

	if execErr != nil {
		o.Class = classifyError(ctx, execErr)
	} else {
		o.Class = metrics.ClassOK
	}
	if q.op == policy.OpWrite {
		r.writes++
	} else {
		r.reads++
	}
	rec.Record(&o)
	return nil
}

// execute runs a query's statements, in a transaction when it is a tx unit so the
// whole thing is timed and committed as one.
func (r *Runner) execute(ctx context.Context, conn *sql.Conn, q *query, tctx *template.Context) error {
	if !q.isTx {
		return r.runStatement(ctx, conn, nil, q, q.stmts[0], tctx)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	for _, st := range q.stmts {
		if err := r.runStatement(ctx, conn, tx, q, st, tctx); err != nil {
			// The rollback error is deliberately not returned: the statement failure is
			// what happened, and reporting a rollback problem instead would hide it.
			if rbErr := tx.Rollback(); rbErr != nil {
				r.deps.Logger.Debug("rolling back", "query", q.name, "err", rbErr)
			}
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

func (r *Runner) runStatement(ctx context.Context, conn *sql.Conn, tx *sql.Tx, q *query, st *statement, tctx *template.Context) error {
	text := st.text
	if text == "" {
		rendered, err := st.sql.Render(tctx)
		if err != nil {
			return err
		}
		text = r.comment(q.name) + rendered
	}

	args := make([]any, 0, len(st.args))
	for _, a := range st.args {
		v, err := a.RenderValue(tctx)
		if err != nil {
			return err
		}
		args = append(args, v)
	}

	if q.op == policy.OpWrite {
		return r.exec(ctx, conn, tx, st, text, args)
	}
	return r.query(ctx, conn, tx, st, text, args)
}

func (r *Runner) exec(ctx context.Context, conn *sql.Conn, tx *sql.Tx, st *statement, text string, args []any) error {
	var err error
	switch {
	case tx != nil && st.prepped != nil:
		_, err = tx.StmtContext(ctx, st.prepped).ExecContext(ctx, args...)
	case tx != nil:
		_, err = tx.ExecContext(ctx, text, args...)
	case st.prepped != nil:
		_, err = st.prepped.ExecContext(ctx, args...)
	default:
		_, err = conn.ExecContext(ctx, text, args...)
	}
	if err != nil {
		return fmt.Errorf("executing: %w", err)
	}
	return nil
}

// query runs a read and consumes every row.
//
// Consuming the rows is the point: a database can return a cursor almost instantly and
// do the real work as the rows are pulled. A read that never iterates would measure the
// planning and none of the execution, and would report a comfortable latency for a
// query that is in fact expensive.
func (r *Runner) query(ctx context.Context, conn *sql.Conn, tx *sql.Tx, st *statement, text string, args []any) error {
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case tx != nil && st.prepped != nil:
		rows, err = tx.StmtContext(ctx, st.prepped).QueryContext(ctx, args...)
	case tx != nil:
		rows, err = tx.QueryContext(ctx, text, args...)
	case st.prepped != nil:
		rows, err = st.prepped.QueryContext(ctx, args...)
	default:
		rows, err = conn.QueryContext(ctx, text, args...)
	}
	if err != nil {
		return fmt.Errorf("querying: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			r.deps.Logger.Debug("closing rows", "err", closeErr)
		}
	}()

	for rows.Next() { //nolint:revive // draining the cursor is the work being measured
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading rows: %w", err)
	}
	return nil
}

// poolClass distinguishes failing to get a connection from failing to use one. A pool
// timeout is our limit, not the server's, and must not be recorded as a database error.
func poolClass(ctx context.Context, err error) metrics.Class {
	switch {
	case errors.Is(err, context.Canceled):
		return metrics.ClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return metrics.ClassPoolTimeout
	case ctx.Err() != nil:
		return metrics.ClassCanceled
	default:
		return metrics.ClassConnection
	}
}

// classifyError sorts a failure into the taxonomy the abort guard and the validity
// rules read.
func classifyError(ctx context.Context, err error) metrics.Class {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return metrics.ClassTimeout
	case errors.Is(err, context.Canceled):
		return metrics.ClassCanceled
	case errors.Is(err, sql.ErrConnDone), errors.Is(err, errDriverBadConn):
		return metrics.ClassConnection
	}
	if ctx.Err() != nil {
		return metrics.ClassCanceled
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "server closed"),
		strings.Contains(msg, "bad connection"),
		strings.Contains(msg, "no such host"):
		return metrics.ClassConnection
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return metrics.ClassTimeout
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return metrics.ClassTLS
	default:
		// The server answered and said no. That is a query error, which says something
		// about the statement rather than about the database's health.
		return metrics.ClassQueryError
	}
}

// errDriverBadConn is database/sql's sentinel for a connection the driver gave up on.
var errDriverBadConn = errors.New("driver: bad connection")

// Close releases the pool.
func (r *Runner) Close() error {
	for _, q := range r.queries {
		for _, st := range q.stmts {
			if st.prepped != nil {
				if err := st.prepped.Close(); err != nil {
					r.deps.Logger.Debug("closing a prepared statement", "err", err)
				}
			}
		}
	}
	if err := r.db.Close(); err != nil {
		return fmt.Errorf("closing the database pool: %w", err)
	}
	return nil
}

var _ runner.Runner = (*Runner)(nil)
