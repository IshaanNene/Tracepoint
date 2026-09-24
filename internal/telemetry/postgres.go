package telemetry

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/runner/sqlrun"
)

// postgres samples pg_stat_activity, pg_locks and pg_stat_database.
//
// Keys:
//
//	sessions_active        client backends in state active (excluding this sampler)
//	sessions_idle_in_tx    client backends idle inside a transaction
//	sessions_waiting_lock  client backends whose wait_event_type is Lock
//	sessions_tracepoint    client backends whose application_name marks them as ours
//	sessions_other         client backends that are not ours: the application's
//	locks_waiting          lock requests not yet granted
//	commits, rollbacks, deadlocks, temp_bytes   deltas since the previous sample
//	cache_hit_ratio        buffer hits / (hits + reads) over the interval
type postgres struct {
	spec  Spec
	db    *sql.DB
	conn  *sql.Conn
	now   func() time.Time
	c     counters
	limit string
}

func newPostgres(spec Spec) (Sampler, error) {
	if spec.DSN == "" {
		return nil, errs.New(errs.CodeConfigMissingField, "the postgres sampler has no dsn").
			WithPath("/telemetry/postgres/dsn").
			WithHint("set telemetry.postgres.dsn, or use it with a db section whose driver is postgres")
	}
	return &postgres{spec: spec, now: spec.now()}, nil
}

func (p *postgres) Name() string { return "postgres" }

func (p *postgres) Open(ctx context.Context) error {
	dsn, err := sqlrun.TagDSN("postgres", p.spec.DSN, "tracepoint-telemetry/"+p.spec.RunID)
	if err != nil {
		return err
	}
	driver, _ := sqlrun.SQLDriver("postgres")
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fmt.Errorf("opening the sampling connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("connecting: %w", err)
	}
	// Read-only for the life of the session: a sampler has no business writing, and
	// this makes that a property of the connection rather than of the queries.
	if _, roErr := conn.ExecContext(ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY"); roErr != nil {
		_ = conn.Close()
		_ = db.Close()
		return fmt.Errorf("making the sampling session read-only: %w", roErr)
	}
	var full bool
	err = conn.QueryRowContext(ctx, `SELECT COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = current_user), false)
		OR pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER')`).Scan(&full)
	if err != nil {
		_ = conn.Close()
		_ = db.Close()
		return fmt.Errorf("checking statistics privileges: %w", err)
	}
	if !full {
		p.limit = "limited: without pg_read_all_stats, other roles' sessions show no state or wait event; GRANT pg_read_all_stats TO the sampling user"
	}
	p.db, p.conn = db, conn
	return nil
}

func (p *postgres) Limitation() string { return p.limit }

const postgresSampleSQL = `
SELECT
  count(*) FILTER (WHERE a.state = 'active'),
  count(*) FILTER (WHERE a.state LIKE 'idle in transaction%'),
  count(*) FILTER (WHERE a.wait_event_type = 'Lock'),
  count(*) FILTER (WHERE a.application_name LIKE 'tracepoint/%'),
  count(*) FILTER (WHERE a.application_name NOT LIKE 'tracepoint%'),
  (SELECT count(*) FROM pg_locks WHERE NOT granted),
  d.xact_commit, d.xact_rollback, d.blks_read, d.blks_hit, d.deadlocks, d.temp_bytes
FROM pg_stat_database d
LEFT JOIN pg_stat_activity a
  ON a.backend_type = 'client backend' AND a.pid <> pg_backend_pid()
WHERE d.datname = current_database()
GROUP BY d.xact_commit, d.xact_rollback, d.blks_read, d.blks_hit, d.deadlocks, d.temp_bytes`

func (p *postgres) Sample(ctx context.Context, _ time.Duration) (Sample, error) {
	var (
		active, idleTx, waitLock, ours, others, locks   int64
		commit, rollback, blksRead, blksHit, dead, temp float64
	)
	err := p.conn.QueryRowContext(ctx, postgresSampleSQL).Scan(
		&active, &idleTx, &waitLock, &ours, &others, &locks,
		&commit, &rollback, &blksRead, &blksHit, &dead, &temp)
	if err != nil {
		return nil, fmt.Errorf("reading statistics: %w", err)
	}
	s := Sample{
		"sessions_active":       active,
		"sessions_idle_in_tx":   idleTx,
		"sessions_waiting_lock": waitLock,
		"sessions_tracepoint":   ours,
		"sessions_other":        others,
		"locks_waiting":         locks,
	}
	d, _, ok := p.c.step(p.now(), map[string]float64{
		"commits": commit, "rollbacks": rollback, "blks_read": blksRead,
		"blks_hit": blksHit, "deadlocks": dead, "temp_bytes": temp,
	})
	if ok {
		for _, k := range []string{"commits", "rollbacks", "deadlocks", "temp_bytes"} {
			if v, has := d[k]; has {
				s[k] = v
			}
		}
		if r, has := ratio(d["blks_hit"], d["blks_hit"]+d["blks_read"]); has {
			s["cache_hit_ratio"] = round3(r)
		}
	}
	return s, nil
}

// Finish reads the top statements by total execution time, when asked to and when the
// extension is installed. Query text is normalised by the server, so it carries
// placeholders rather than literal values.
func (p *postgres) Finish(ctx context.Context) ([]map[string]any, error) {
	if !p.spec.Statements || p.conn == nil {
		return nil, nil
	}
	rows, err := p.conn.QueryContext(ctx, `
SELECT queryid::text, left(regexp_replace(query, '\s+', ' ', 'g'), 200), calls,
       total_exec_time, mean_exec_time, rows
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
ORDER BY total_exec_time DESC
LIMIT 10`)
	if err != nil {
		return nil, fmt.Errorf("reading pg_stat_statements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var (
			id, query       string
			calls, nrows    int64
			total, meanTime float64
		)
		if err := rows.Scan(&id, &query, &calls, &total, &meanTime, &nrows); err != nil {
			return nil, fmt.Errorf("scanning pg_stat_statements: %w", err)
		}
		out = append(out, map[string]any{
			"queryid": id, "query": errs.CleanUntrusted(query), "calls": calls,
			"total_ms": round3(total), "mean_ms": round3(meanTime), "rows": nrows,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading pg_stat_statements: %w", err)
	}
	return out, nil
}

func (p *postgres) Close() error {
	var first error
	if p.conn != nil {
		first = p.conn.Close()
	}
	if p.db != nil {
		if err := p.db.Close(); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return fmt.Errorf("closing the postgres sampler: %w", first)
	}
	return nil
}
