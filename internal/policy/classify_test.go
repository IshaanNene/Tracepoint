package policy_test

import (
	"strings"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/policy"
)

func TestClassifySQL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		sql       string
		op        policy.Op
		dangerous string
		certain   bool
	}{
		{"select", "SELECT * FROM items WHERE id = $1", policy.OpRead, "", true},
		{"lowercase select", "select id from items", policy.OpRead, "", true},
		{"leading whitespace", "\n\t  SELECT 1", policy.OpRead, "", true},
		{"show", "SHOW GLOBAL STATUS", policy.OpRead, "", true},
		{"explain", "EXPLAIN ANALYZE SELECT 1", policy.OpRead, "", true},
		{"insert", "INSERT INTO items (title) VALUES ($1)", policy.OpWrite, "", true},
		{"update", "UPDATE items SET title = $1 WHERE id = $2", policy.OpWrite, "", true},
		{"delete", "DELETE FROM items WHERE id = $1", policy.OpWrite, "", true},
		{"upsert", "INSERT INTO t VALUES (1) ON CONFLICT DO UPDATE SET x = 2", policy.OpWrite, "", true},
		{"drop", "DROP TABLE items", policy.OpWrite, "DROP", true},
		{"truncate", "TRUNCATE items", policy.OpWrite, "TRUNCATE", true},
		{"alter", "ALTER TABLE items ADD COLUMN x int", policy.OpWrite, "ALTER", true},
		{"create", "CREATE TABLE t (id int)", policy.OpWrite, "CREATE", true},
		{"grant", "GRANT ALL ON items TO app", policy.OpWrite, "GRANT", true},
		{"transaction control is neither", "BEGIN", policy.OpRead, "", true},
		{"commit", "COMMIT", policy.OpRead, "", true},
		{"set is a session setting", "SET application_name = 'x'", policy.OpRead, "", true},
		{"empty", "", policy.OpRead, "", true},
		{"whitespace only", "   \n  ", policy.OpRead, "", true},
		{"unrecognised fails closed", "FROBNICATE items", policy.OpWrite, "", false},

		// A CTE hides the real verb, so the classifier has to look past it.
		{"cte over select", "WITH recent AS (SELECT * FROM items) SELECT * FROM recent", policy.OpRead, "", true},
		{"cte over delete", "WITH old AS (SELECT id FROM items) DELETE FROM items WHERE id IN (SELECT id FROM old)", policy.OpWrite, "", true},
		{"cte over insert", "WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", policy.OpWrite, "", true},

		// SELECT ... INTO creates a table; SELECT is not always a read.
		{"select into", "SELECT * INTO backup FROM items", policy.OpWrite, "", true},

		// A multi-statement string is judged on its most serious part.
		{"multi statement", "SELECT 1; DELETE FROM items", policy.OpWrite, "", true},
		{"multi with a drop", "SELECT 1; DROP TABLE items", policy.OpWrite, "DROP", true},
		{"trailing semicolon", "SELECT 1;", policy.OpRead, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := policy.ClassifySQL(tc.sql)
			if got.Op != tc.op {
				t.Errorf("op = %s, want %s", got.Op, tc.op)
			}
			if got.Dangerous != tc.dangerous {
				t.Errorf("dangerous = %q, want %q", got.Dangerous, tc.dangerous)
			}
			if got.Certain != tc.certain {
				t.Errorf("certain = %v, want %v", got.Certain, tc.certain)
			}
		})
	}
}

// A keyword inside a string literal or a comment is data, not a statement. Refusing a
// SELECT because its WHERE clause mentions a table called "drop_log" would be wrong,
// and would teach people to grant allow_dangerous to get their reads through.
func TestClassifySQLIgnoresLiteralsAndComments(t *testing.T) {
	t.Parallel()
	cases := []string{
		`SELECT * FROM audit WHERE action = 'DROP TABLE users'`,
		`SELECT * FROM t WHERE note = "TRUNCATE everything"`,
		`SELECT 1 -- DROP TABLE users`,
		`/* tracepoint_run, by-id */ SELECT * FROM items WHERE id = $1`,
		"SELECT 1 /* DELETE FROM items */",
		"-- DROP TABLE users\nSELECT 1",
	}
	for _, sql := range cases {
		t.Run(sql[:min(len(sql), 40)], func(t *testing.T) {
			t.Parallel()
			got := policy.ClassifySQL(sql)
			if got.Op != policy.OpRead {
				t.Errorf("%q classified as %s; a keyword in a literal or comment is data", sql, got.Op)
			}
			if got.Dangerous != "" {
				t.Errorf("%q flagged as dangerous (%s)", sql, got.Dangerous)
			}
		})
	}

	// The reverse must also hold: a comment must not be able to hide a real statement.
	if got := policy.ClassifySQL("/* SELECT */ DROP TABLE users"); got.Dangerous != "DROP" {
		t.Errorf("a comment hid a DROP: %+v", got)
	}
}

func TestClassifyRedis(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		cmd       []string
		op        policy.Op
		dangerous string
		blocking  string
		certain   bool
	}{
		{"get", []string{"GET", "session:1"}, policy.OpRead, "", "", true},
		{"lowercase", []string{"get", "k"}, policy.OpRead, "", "", true},
		{"ping", []string{"PING"}, policy.OpRead, "", "", true},
		{"scan", []string{"SCAN", "0"}, policy.OpRead, "", "", true},
		{"set", []string{"SET", "k", "v"}, policy.OpWrite, "", "", true},
		{"del", []string{"DEL", "k"}, policy.OpWrite, "", "", true},
		{"incr", []string{"INCR", "counter"}, policy.OpWrite, "", "", true},
		{"eval", []string{"EVAL", "return 1", "0"}, policy.OpWrite, "", "", true},

		{"flushall", []string{"FLUSHALL"}, policy.OpWrite, "FLUSHALL", "", true},
		{"flushdb", []string{"FLUSHDB"}, policy.OpWrite, "FLUSHDB", "", true},
		{"shutdown", []string{"SHUTDOWN"}, policy.OpWrite, "SHUTDOWN", "", true},
		{"debug sleep", []string{"DEBUG", "SLEEP", "1"}, policy.OpWrite, "DEBUG", "", true},

		// Dangerous only in some subcommands: reading the config is harmless,
		// rewriting it under a running system is not.
		{"config get", []string{"CONFIG", "GET", "maxmemory"}, policy.OpRead, "", "", true},
		{"config set", []string{"CONFIG", "SET", "maxmemory", "0"}, policy.OpWrite, "CONFIG SET", "", true},
		{"script load", []string{"SCRIPT", "LOAD", "return 1"}, policy.OpRead, "", "", true},
		{"script flush", []string{"SCRIPT", "FLUSH"}, policy.OpWrite, "SCRIPT FLUSH", "", true},
		{"client kill", []string{"CLIENT", "KILL", "ID", "3"}, policy.OpWrite, "CLIENT KILL", "", true},
		{"client setname", []string{"CLIENT", "SETNAME", "tracepoint"}, policy.OpRead, "", "", true},

		// O(N) blockers are allowed but reported: they distort the target and the
		// measurement alike.
		{"keys", []string{"KEYS", "*"}, policy.OpRead, "", "KEYS", true},
		{"sort reads", []string{"SORT", "mylist"}, policy.OpRead, "", "SORT", true},
		{"sort with store writes", []string{"SORT", "mylist", "STORE", "dest"}, policy.OpWrite, "", "SORT", true},
		{"smembers", []string{"SMEMBERS", "big"}, policy.OpRead, "", "SMEMBERS", true},

		{"unknown fails closed", []string{"FROBNICATE", "x"}, policy.OpWrite, "", "", false},
		{"empty", nil, policy.OpRead, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := policy.ClassifyRedis(tc.cmd)
			if got.Op != tc.op {
				t.Errorf("op = %s, want %s", got.Op, tc.op)
			}
			if got.Dangerous != tc.dangerous {
				t.Errorf("dangerous = %q, want %q", got.Dangerous, tc.dangerous)
			}
			if got.Blocking != tc.blocking {
				t.Errorf("blocking = %q, want %q", got.Blocking, tc.blocking)
			}
			if got.Certain != tc.certain {
				t.Errorf("certain = %v, want %v", got.Certain, tc.certain)
			}
		})
	}
}

// Every destructive keyword the spec names in §8 must actually be caught. This is the
// list a reader of the spec would check against.
func TestEverySpecifiedDangerousKeywordIsCaught(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{"DROP TABLE t", "TRUNCATE t", "ALTER TABLE t ADD x int"} {
		if got := policy.ClassifySQL(sql); got.Dangerous == "" {
			t.Errorf("%q was not flagged as dangerous", sql)
		}
	}
	for _, cmd := range [][]string{
		{"FLUSHALL"}, {"FLUSHDB"}, {"SHUTDOWN"},
		{"CONFIG", "SET", "x", "1"}, {"DEBUG", "SLEEP", "1"}, {"SCRIPT", "FLUSH"},
	} {
		if got := policy.ClassifyRedis(cmd); got.Dangerous == "" {
			t.Errorf("%v was not flagged as dangerous", cmd)
		}
	}
}

// Both classifiers are fuzz targets in §10: neither may panic, and neither may report
// a read for something it did not recognise.
func FuzzClassifySQL(f *testing.F) {
	for _, s := range []string{
		"SELECT 1", "DROP TABLE t", "", ";", ";;;", "'", `"`, "/*", "--", "/*/",
		"WITH a AS (SELECT 1) DELETE FROM t", "SELECT '--' FROM t", "INSERT",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		got := policy.ClassifySQL(sql)
		if got.Op != policy.OpRead && got.Op != policy.OpWrite {
			t.Fatalf("ClassifySQL(%q) produced an unknown op %q", sql, got.Op)
		}
		if !got.Certain && got.Op != policy.OpWrite {
			t.Fatalf("ClassifySQL(%q) was uncertain but did not fail closed: %+v", sql, got)
		}
	})
}

func FuzzClassifyRedis(f *testing.F) {
	for _, s := range []string{"GET k", "SET k v", "FLUSHALL", "", "CONFIG", "CONFIG SET", "keys *"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		got := policy.ClassifyRedis(strings.Fields(line))
		if got.Op != policy.OpRead && got.Op != policy.OpWrite {
			t.Fatalf("ClassifyRedis(%q) produced an unknown op %q", line, got.Op)
		}
		if !got.Certain && got.Op != policy.OpWrite {
			t.Fatalf("ClassifyRedis(%q) was uncertain but did not fail closed: %+v", line, got)
		}
	})
}
