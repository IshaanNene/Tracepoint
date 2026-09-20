package policy

import (
	"strings"
)

// Op is what a statement or command does to the data.
type Op string

// Operation kinds.
const (
	OpRead  Op = "read"
	OpWrite Op = "write"
)

// Classification is what a statement was judged to be.
type Classification struct {
	Op Op
	// Dangerous names the destructive or administrative keyword found, if any.
	Dangerous string
	// Blocking names an operation that blocks the server for time proportional to the
	// data size. Not refused, but it distorts both the target and the measurement.
	Blocking string
	// Certain is false when the classifier did not recognise the statement. Unknown
	// means write, because guessing "read" on something that turns out to modify data
	// is the failure that cannot be undone.
	Certain bool
}

// sqlReadVerbs are statements that only observe.
//
//nolint:gochecknoglobals // Fixed lookup tables, never mutated after init.
var sqlReadVerbs = map[string]bool{
	"SELECT": true, "SHOW": true, "EXPLAIN": true, "DESCRIBE": true, "DESC": true,
	"VALUES": true, "TABLE": true, "ANALYZE": true, "PRAGMA": true,
}

//nolint:gochecknoglobals // Fixed lookup table.
var sqlWriteVerbs = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "UPSERT": true,
	"REPLACE": true, "COPY": true, "CALL": true, "DO": true, "EXEC": true, "EXECUTE": true,
}

// sqlDangerousVerbs change or destroy schema, data or server state irreversibly.
//
//nolint:gochecknoglobals // Fixed lookup table.
var sqlDangerousVerbs = map[string]bool{
	"DROP": true, "TRUNCATE": true, "ALTER": true, "CREATE": true, "RENAME": true,
	"GRANT": true, "REVOKE": true, "VACUUM": true, "REINDEX": true, "CLUSTER": true,
	"SHUTDOWN": true, "RESET": true, "KILL": true, "LOCK": true,
}

// ClassifySQL decides what a statement does.
//
// Classification is by the statement's leading verb, after comments are stripped, and
// never by searching the whole text: a SELECT whose WHERE clause contains the word
// "drop" in a string literal must not be refused. A multi-statement string is judged
// on its most serious part.
//
// It is a heuristic, and the configuration's own `type: read|write` overrides it. What
// the heuristic guarantees is the direction of its mistakes: anything it does not
// recognise is treated as a write, because being wrong about a read is recoverable and
// being wrong about a write is not.
func ClassifySQL(statement string) Classification {
	worst := Classification{Op: OpRead, Certain: true}

	for _, part := range splitStatements(stripSQLComments(statement)) {
		c := classifyOneSQL(part)
		worst = merge(worst, c)
	}
	return worst
}

func classifyOneSQL(statement string) Classification {
	words := fields(statement)
	if len(words) == 0 {
		// An empty statement does nothing, but it is also not something we understood.
		return Classification{Op: OpRead, Certain: true}
	}
	verb := strings.ToUpper(words[0])

	// A CTE hides the real verb behind the WITH clause, so "WITH x AS (...) DELETE"
	// must not read as a SELECT. Look past the clause for a statement verb.
	if verb == "WITH" {
		for _, w := range words[1:] {
			u := strings.ToUpper(w)
			if sqlWriteVerbs[u] {
				return Classification{Op: OpWrite, Certain: true}
			}
			if sqlDangerousVerbs[u] {
				return Classification{Op: OpWrite, Dangerous: u, Certain: true}
			}
		}
		return Classification{Op: OpRead, Certain: true}
	}

	switch {
	case sqlDangerousVerbs[verb]:
		// CREATE and ALTER of a temporary object inside a test are still schema
		// changes against a real database; treating them as ordinary writes would let
		// a stray DDL through on an allow_writes grant alone.
		return Classification{Op: OpWrite, Dangerous: verb, Certain: true}
	case sqlWriteVerbs[verb]:
		return Classification{Op: OpWrite, Certain: true}
	case sqlReadVerbs[verb]:
		// SELECT ... INTO and SELECT ... FOR UPDATE both write or lock.
		if hasWord(words, "INTO") && verb == "SELECT" {
			return Classification{Op: OpWrite, Certain: true}
		}
		return Classification{Op: OpRead, Certain: true}
	case verb == "BEGIN", verb == "START", verb == "COMMIT", verb == "ROLLBACK", verb == "SAVEPOINT", verb == "SET":
		// Transaction control and session settings do not themselves touch data.
		return Classification{Op: OpRead, Certain: true}
	default:
		return Classification{Op: OpWrite, Certain: false}
	}
}

func merge(a, b Classification) Classification {
	out := a
	if b.Op == OpWrite {
		out.Op = OpWrite
	}
	if b.Dangerous != "" && out.Dangerous == "" {
		out.Dangerous = b.Dangerous
	}
	if b.Blocking != "" && out.Blocking == "" {
		out.Blocking = b.Blocking
	}
	if !b.Certain {
		out.Certain = false
	}
	return out
}

// stripSQLComments removes -- line comments and /* */ block comments, so neither can
// hide a verb nor introduce a fake one. TracePoint adds a sqlcommenter-style comment of
// its own, which this also removes before classifying.
func stripSQLComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				return b.String()
			}
			i += end
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 4
			b.WriteByte(' ')
		case s[i] == '\'' || s[i] == '"':
			// A quoted literal is copied as one opaque unit so its contents can never
			// be mistaken for a keyword.
			quote := s[i]
			b.WriteByte(' ')
			i++
			for i < len(s) && s[i] != quote {
				i++
			}
			i++
			b.WriteByte(' ')
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// splitStatements separates a multi-statement string on semicolons.
func splitStatements(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '(', ')', ',':
			return true
		default:
			return false
		}
	})
}

func hasWord(words []string, want string) bool {
	for _, w := range words {
		if strings.EqualFold(w, want) {
			return true
		}
	}
	return false
}

// redisReads are commands that only observe.
//
//nolint:gochecknoglobals // Fixed lookup table.
var redisReads = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "GETRANGE": true, "SUBSTR": true,
	"EXISTS": true, "TTL": true, "PTTL": true, "TYPE": true, "OBJECT": true, "DUMP": true,
	"HGET": true, "HMGET": true, "HGETALL": true, "HKEYS": true, "HVALS": true, "HLEN": true, "HEXISTS": true, "HRANDFIELD": true,
	"LRANGE": true, "LLEN": true, "LINDEX": true, "LPOS": true,
	"SMEMBERS": true, "SCARD": true, "SISMEMBER": true, "SMISMEMBER": true, "SRANDMEMBER": true, "SINTER": true, "SUNION": true, "SDIFF": true,
	"ZRANGE": true, "ZRANGEBYSCORE": true, "ZRANGEBYLEX": true, "ZREVRANGE": true, "ZSCORE": true, "ZMSCORE": true,
	"ZCARD": true, "ZCOUNT": true, "ZRANK": true, "ZREVRANK": true, "ZRANDMEMBER": true,
	"BITCOUNT": true, "BITPOS": true, "GETBIT": true, "PFCOUNT": true,
	"XLEN": true, "XRANGE": true, "XREVRANGE": true, "XINFO": true,
	"GEOPOS": true, "GEODIST": true, "GEOSEARCH": true,
	"PING": true, "ECHO": true, "TIME": true, "DBSIZE": true, "INFO": true, "LASTSAVE": true,
	"SCAN": true, "HSCAN": true, "SSCAN": true, "ZSCAN": true, "RANDOMKEY": true, "KEYS": true,
	"MEMORY": true, "LATENCY": true, "COMMAND": true, "LOLWUT": true,
}

// redisDangerous are destructive or administrative commands.
//
//nolint:gochecknoglobals // Fixed lookup table.
var redisDangerous = map[string]bool{
	"FLUSHALL": true, "FLUSHDB": true, "SHUTDOWN": true, "DEBUG": true,
	"REPLICAOF": true, "SLAVEOF": true, "MIGRATE": true, "SWAPDB": true,
	"MODULE": true, "FAILOVER": true, "RESET": true, "BGREWRITEAOF": true, "SAVE": true,
}

// redisDangerousSub are commands dangerous only in certain subcommands - CONFIG GET is
// harmless, CONFIG SET reconfigures the server under a running system.
//
//nolint:gochecknoglobals // Fixed lookup table.
var redisDangerousSub = map[string]map[string]bool{
	"CONFIG":   {"SET": true, "RESETSTAT": true, "REWRITE": true},
	"SCRIPT":   {"FLUSH": true},
	"CLUSTER":  {"RESET": true, "FORGET": true, "FAILOVER": true, "SETSLOT": true},
	"CLIENT":   {"KILL": true, "PAUSE": true, "UNPAUSE": true, "NO-EVICT": true},
	"ACL":      {"SETUSER": true, "DELUSER": true, "LOAD": true, "SAVE": true},
	"FUNCTION": {"FLUSH": true, "DELETE": true},
}

// redisBlocking are commands whose cost grows with the size of the data and which block
// the server while they run. Not refused - sometimes they are the point - but they
// distort both the target and the measurement, so they are reported.
//
//nolint:gochecknoglobals // Fixed lookup table.
var redisBlocking = map[string]bool{
	"KEYS": true, "SORT": true, "SINTERSTORE": true, "SUNIONSTORE": true, "SDIFFSTORE": true,
	"SMEMBERS": true, "HGETALL": true, "LRANGE": true, "ZUNIONSTORE": true, "ZINTERSTORE": true,
}

// ClassifyRedis decides what a Redis command does.
//
// Unlike SQL this is a lookup rather than a parse, because Redis commands are a closed
// vocabulary. A command not in the table is treated as a write and marked uncertain:
// the set grows with every Redis release, and assuming an unfamiliar command is
// harmless is the assumption that loses data.
func ClassifyRedis(args []string) Classification {
	if len(args) == 0 {
		return Classification{Op: OpRead, Certain: true}
	}
	cmd := strings.ToUpper(strings.TrimSpace(args[0]))
	sub := ""
	if len(args) > 1 {
		sub = strings.ToUpper(strings.TrimSpace(args[1]))
	}

	c := Classification{Certain: true}
	if redisBlocking[cmd] {
		c.Blocking = cmd
	}

	// SORT reads unless it is asked to store its output, which most SORT calls are not.
	// Treating every SORT as a write would make a read-only configuration need
	// allow_writes, and a grant given for the wrong reason is a grant that stays.
	if cmd == "SORT" {
		c.Op = OpRead
		for _, a := range args[1:] {
			if strings.EqualFold(a, "STORE") {
				c.Op = OpWrite
				break
			}
		}
		return c
	}

	switch {
	case redisDangerous[cmd]:
		c.Op, c.Dangerous = OpWrite, cmd
	case redisDangerousSub[cmd] != nil:
		if redisDangerousSub[cmd][sub] {
			c.Op, c.Dangerous = OpWrite, cmd+" "+sub
		} else {
			c.Op = OpRead
		}
	case redisReads[cmd]:
		c.Op = OpRead
	case knownRedisWrite(cmd):
		c.Op = OpWrite
	default:
		c.Op, c.Certain = OpWrite, false
	}
	return c
}

// knownRedisWrite recognises the ordinary mutating commands. Kept as a prefix and set
// check rather than an exhaustive list, because the mutating vocabulary is large and
// regular while the read vocabulary is the one worth enumerating precisely.
func knownRedisWrite(cmd string) bool {
	switch cmd {
	case "SET", "SETNX", "SETEX", "PSETEX", "MSET", "MSETNX", "GETSET", "GETDEL", "GETEX",
		"APPEND", "SETRANGE", "SETBIT", "BITFIELD", "BITOP",
		"INCR", "INCRBY", "INCRBYFLOAT", "DECR", "DECRBY",
		"DEL", "UNLINK", "EXPIRE", "PEXPIRE", "EXPIREAT", "PEXPIREAT", "PERSIST", "RENAME", "RENAMENX",
		"HSET", "HSETNX", "HMSET", "HDEL", "HINCRBY", "HINCRBYFLOAT",
		"LPUSH", "RPUSH", "LPUSHX", "RPUSHX", "LPOP", "RPOP", "LSET", "LINSERT", "LREM", "LTRIM", "RPOPLPUSH", "LMOVE",
		"SADD", "SREM", "SPOP", "SMOVE", "SINTERSTORE", "SUNIONSTORE", "SDIFFSTORE",
		"ZADD", "ZREM", "ZINCRBY", "ZPOPMIN", "ZPOPMAX", "ZREMRANGEBYSCORE", "ZREMRANGEBYRANK", "ZREMRANGEBYLEX",
		"ZUNIONSTORE", "ZINTERSTORE", "ZDIFFSTORE",
		"PFADD", "PFMERGE", "SETEXPIRE", "COPY", "RESTORE",
		"XADD", "XDEL", "XTRIM", "XGROUP", "XACK", "XCLAIM", "XAUTOCLAIM",
		"GEOADD", "PUBLISH", "SPUBLISH", "EVAL", "EVALSHA", "FCALL", "MULTI", "EXEC", "DISCARD", "WATCH", "UNWATCH":
		return true
	default:
		return false
	}
}
