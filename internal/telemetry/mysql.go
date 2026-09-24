package telemetry

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/runner/sqlrun"
)

// mysql samples SHOW GLOBAL STATUS.
//
// Keys:
//
//	threads_running, threads_connected   gauges
//	row_lock_waits, row_lock_time_ms     InnoDB row-lock deltas since the previous sample
//	buffer_pool_hit_ratio                1 - disk reads / read requests over the interval
//	queries_per_sec                      statements executed per second over the interval
type mysql struct {
	spec Spec
	db   *sql.DB
	conn *sql.Conn
	now  func() time.Time
	c    counters
}

// mysqlStatus are the variables read, and the key each becomes.
//
//nolint:gochecknoglobals // A fixed table, never mutated.
var mysqlStatus = map[string]string{
	"Threads_running":                  "threads_running",
	"Threads_connected":                "threads_connected",
	"Innodb_row_lock_waits":            "row_lock_waits",
	"Innodb_row_lock_time":             "row_lock_time_ms",
	"Innodb_buffer_pool_reads":         "bp_reads",
	"Innodb_buffer_pool_read_requests": "bp_requests",
	"Questions":                        "questions",
}

// mysqlStatusSQL reads exactly the variables in mysqlStatus. It is a constant rather
// than built from the map so that no query text is ever assembled at run time.
const mysqlStatusSQL = `SHOW GLOBAL STATUS WHERE Variable_name IN (
  'Threads_running', 'Threads_connected', 'Innodb_row_lock_waits', 'Innodb_row_lock_time',
  'Innodb_buffer_pool_reads', 'Innodb_buffer_pool_read_requests', 'Questions')`

func newMySQL(spec Spec) (Sampler, error) {
	if spec.DSN == "" {
		return nil, errs.New(errs.CodeConfigMissingField, "the mysql sampler has no dsn").
			WithPath("/telemetry/mysql/dsn").
			WithHint("set telemetry.mysql.dsn, or use it with a db section whose driver is mysql")
	}
	return &mysql{spec: spec, now: spec.now()}, nil
}

func (m *mysql) Name() string { return "mysql" }

func (m *mysql) Open(ctx context.Context) error {
	driver, _ := sqlrun.SQLDriver("mysql")
	db, err := sql.Open(driver, m.spec.DSN)
	if err != nil {
		return fmt.Errorf("opening the sampling connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("connecting: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return fmt.Errorf("making the sampling session read-only: %w", err)
	}
	m.db, m.conn = db, conn
	return nil
}

func (m *mysql) Sample(ctx context.Context, _ time.Duration) (Sample, error) {
	rows, err := m.conn.QueryContext(ctx, mysqlStatusSQL)
	if err != nil {
		return nil, fmt.Errorf("reading global status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	values := map[string]float64{}
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("scanning global status: %w", err)
		}
		if key, ok := mysqlStatus[name]; ok {
			if v, perr := strconv.ParseFloat(raw, 64); perr == nil {
				values[key] = v
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading global status: %w", err)
	}

	s := Sample{}
	for _, k := range []string{"threads_running", "threads_connected"} {
		if v, ok := values[k]; ok {
			s[k] = v
		}
	}
	d, elapsed, ok := m.c.step(m.now(), map[string]float64{
		"row_lock_waits": values["row_lock_waits"], "row_lock_time_ms": values["row_lock_time_ms"],
		"bp_reads": values["bp_reads"], "bp_requests": values["bp_requests"], "questions": values["questions"],
	})
	if ok {
		for _, k := range []string{"row_lock_waits", "row_lock_time_ms"} {
			if v, has := d[k]; has {
				s[k] = v
			}
		}
		if r, has := ratio(d["bp_reads"], d["bp_requests"]); has {
			s["buffer_pool_hit_ratio"] = round3(1 - r)
		}
		if secs := elapsed.Seconds(); secs > 0 {
			s["queries_per_sec"] = round3(d["questions"] / secs)
		}
	}
	return s, nil
}

func (m *mysql) Close() error {
	var first error
	if m.conn != nil {
		first = m.conn.Close()
	}
	if m.db != nil {
		if err := m.db.Close(); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return fmt.Errorf("closing the mysql sampler: %w", first)
	}
	return nil
}
