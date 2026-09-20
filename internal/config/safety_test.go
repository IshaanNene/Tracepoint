package config_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
)

func ptr[T any](v T) *T { return &v }

// dump renders a configuration the way an artifact would, through both encoders, so a
// secret surviving in either is caught.
func dump(t *testing.T, c *config.Config) string {
	t.Helper()
	asJSON, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("encoding as JSON: %v", err)
	}
	asYAML, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("encoding as YAML: %v", err)
	}
	return string(asJSON) + "\n" + string(asYAML)
}

const dbConfig = `
version: 1
run: { duration: 10s }
db:
  driver: postgres
  dsn: "postgres://app:hunter2@10.0.0.5:5432/shop"
  executor: { rate: 10 }
  queries:
    - { name: by-id, type: read, sql: "SELECT * FROM items WHERE id = $1", args: ["{{randInt 1 100}}"] }
`

func TestApplySet(t *testing.T) {
	t.Parallel()

	t.Run("scalar paths", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, minimal, nil)
		if err := cfg.ApplySet([]string{
			"run.duration=90s",
			"run.seed=7",
			"http.executor.rate=250",
			"http.executor.max_in_flight=64",
			"http.base_url=http://example.internal",
			"safety.allow_writes=true",
		}); err != nil {
			t.Fatalf("ApplySet: %v", err)
		}
		if got := cfg.Run.Duration.D(); got != 90*time.Second {
			t.Errorf("duration = %v, want 90s", got)
		}
		if cfg.Run.Seed == nil || *cfg.Run.Seed != 7 {
			t.Errorf("seed = %v, want 7", cfg.Run.Seed)
		}
		if cfg.HTTP.Executor.Rate != 250 || cfg.HTTP.Executor.MaxInFlight != 64 {
			t.Errorf("executor = %+v", cfg.HTTP.Executor)
		}
		if !cfg.Safety.AllowWrites {
			t.Error("allow_writes was not set")
		}
	})

	// A recommendation in a digest addresses a query by name, because that is how a
	// person and an agent both think about it.
	t.Run("a list item addressed by name", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, dbConfig, nil)
		if err := cfg.ApplySet([]string{"db.queries[by-id].weight=5", "db.queries[by-id].type=write"}); err != nil {
			t.Fatalf("ApplySet: %v", err)
		}
		if got := cfg.DB.Queries[0].WeightOr(); got != 5 {
			t.Errorf("weight = %v, want 5", got)
		}
		if cfg.DB.Queries[0].Type != "write" {
			t.Errorf("type = %q", cfg.DB.Queries[0].Type)
		}
	})

	t.Run("a list item addressed by index", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, minimal, nil)
		if err := cfg.ApplySet([]string{"http.requests[0].method=POST"}); err != nil {
			t.Fatalf("ApplySet: %v", err)
		}
		if cfg.HTTP.Requests[0].Method != "POST" {
			t.Errorf("method = %q", cfg.HTTP.Requests[0].Method)
		}
	})

	// A section that does not exist yet is created, so an override does not depend on
	// the configuration happening to have the block already.
	t.Run("a missing section is created", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, dbConfig, nil)
		if cfg.DB.Pool != nil {
			t.Fatal("this fixture should have no pool block")
		}
		if err := cfg.ApplySet([]string{"db.pool.max_open=20"}); err != nil {
			t.Fatalf("ApplySet: %v", err)
		}
		if cfg.DB.Pool == nil || cfg.DB.Pool.MaxOpen != 20 {
			t.Errorf("pool = %+v", cfg.DB.Pool)
		}
	})

	t.Run("lists and maps", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, minimal, nil)
		if err := cfg.ApplySet([]string{
			"safety.allow_targets=a.example.com,b.example.com",
			"run.tags=env:staging",
		}); err != nil {
			t.Fatalf("ApplySet: %v", err)
		}
		if len(cfg.Safety.AllowTargets) != 2 {
			t.Errorf("allow_targets = %v", cfg.Safety.AllowTargets)
		}
		if cfg.Run.Tags["env"] != "staging" {
			t.Errorf("tags = %v", cfg.Run.Tags)
		}
	})

	// An override that quietly does nothing produces a run that looks like it tested
	// the change and did not.
	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name     string
			override string
			code     errs.Code
			hint     string
		}{
			{"no equals sign", "run.duration", errs.CodeSetParse, ""},
			{"empty path", "=5", errs.CodeSetParse, ""},
			{"unknown top-level field", "runn.duration=5s", errs.CodeSetUnknownPath, "run"},
			{"unknown nested field", "run.duratio=5s", errs.CodeSetUnknownPath, "duration"},
			{"unknown item name", "http.requests[nope].method=POST", errs.CodeSetIndexNotFound, "probe"},
			{"index out of range", "http.requests[9].method=POST", errs.CodeSetIndexNotFound, ""},
			{"unclosed bracket", "http.requests[0.method=POST", errs.CodeSetParse, ""},
			{"not a number", "http.executor.rate=fast", errs.CodeSetTypeMismatch, ""},
			{"not a duration", "run.duration=soon", errs.CodeSetTypeMismatch, ""},
			{"not a boolean", "safety.allow_writes=maybe", errs.CodeSetTypeMismatch, ""},
			{"selector on a non-list", "run[0].duration=5s", errs.CodeSetUnknownPath, ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg := mustLoad(t, minimal, nil)
				err := cfg.ApplySet([]string{tc.override})
				if err == nil {
					t.Fatalf("--set %s was accepted", tc.override)
				}
				var typed *errs.Error
				if !errors.As(err, &typed) || typed.Code != tc.code {
					t.Fatalf("code = %v, want %s", err, tc.code)
				}
				if tc.hint != "" && !strings.Contains(typed.Hint, tc.hint) {
					t.Errorf("hint = %q, want it to mention %q", typed.Hint, tc.hint)
				}
			})
		}
	})

	t.Run("every problem is reported at once", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, minimal, nil)
		err := cfg.ApplySet([]string{"nope.a=1", "run.duratio=5s", "run.duration=soon"})
		var typed *errs.Error
		if !errors.As(err, &typed) {
			t.Fatalf("not a coded error: %v", err)
		}
		if len(typed.Causes) != 3 {
			t.Errorf("reported %d problems, want 3", len(typed.Causes))
		}
	})
}

func TestSetPathsAreDiscoverable(t *testing.T) {
	t.Parallel()
	paths := config.SetPaths()
	want := []string{"run.duration", "http.executor.rate", "safety.allow_writes", "db.queries[name].weight"}
	for _, w := range want {
		found := false
		for _, p := range paths {
			if p == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SetPaths() does not list %q", w)
		}
	}
}

// A sentinel secret must appear in no artifact (§8). This is the unit-level half of
// that promise; the end-to-end half runs against a real result document.
func TestRedaction(t *testing.T) {
	t.Parallel()
	const sentinel = "hunter2-do-not-leak"

	cfg := mustLoad(t, `
version: 1
run: { duration: 10s }
http:
  headers: { Authorization: "Bearer `+sentinel+`", X-Trace: keep-me }
  executor: { rate: 1 }
  requests:
    - name: probe
      url: "http://127.0.0.1/"
      headers: { Cookie: "session=`+sentinel+`", Accept: application/json }
db:
  driver: postgres
  dsn: "postgres://app:`+sentinel+`@10.0.0.5:5432/shop"
  executor: { rate: 1 }
  queries:
    - name: q
      sql: "SELECT 1"
      args: [{ value: "`+sentinel+`", secret: true }, "public-value"]
redis:
  addr: "10.0.0.6:6379"
  password: "`+sentinel+`"
  executor: { rate: 1 }
  commands: [{ name: c, cmd: [GET, k] }]
`, nil)

	redacted := cfg.Redacted()
	rendered := dump(t, redacted)
	if strings.Contains(rendered, sentinel) {
		t.Fatalf("the sentinel secret survived redaction:\n%s", rendered)
	}

	// Redaction must remove the secret and keep everything useful. A DSN with the
	// whole string blanked would lose the host and database, which are what say what
	// was tested.
	for _, keep := range []string{"10.0.0.5", "shop", "keep-me", "application/json", "public-value", "10.0.0.6"} {
		if !strings.Contains(rendered, keep) {
			t.Errorf("redaction removed %q, which is not a secret", keep)
		}
	}

	// The original must be untouched: the run still needs to connect.
	if !strings.Contains(cfg.DB.DSN, sentinel) {
		t.Error("redacting mutated the configuration the run is using")
	}
	if cfg.Redis.Password != sentinel {
		t.Error("redacting mutated the redis password in place")
	}
}

func TestRedactDSNShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dsn  string
		keep []string
	}{
		{"postgres url", "postgres://app:secret@db.internal:5432/shop?sslmode=require", []string{"db.internal", "5432", "shop", "sslmode"}},
		{"mysql", "app:secret@tcp(db.internal:3306)/shop?parseTime=true", []string{"db.internal", "3306", "shop"}},
		{"libpq key value", "host=db.internal port=5432 user=app password=secret dbname=shop", []string{"db.internal", "5432", "shop"}},
		{"libpq quoted", "host=db.internal password='sec ret' dbname=shop", []string{"db.internal", "shop"}},
		{"no password", "postgres://app@db.internal:5432/shop", []string{"db.internal", "shop", "app"}},
		{"sqlite path", "file:test.db?cache=shared", []string{"test.db"}},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := config.RedactDSN(tc.dsn)
			if strings.Contains(got, "secret") || strings.Contains(got, "sec ret") {
				t.Errorf("RedactDSN(%q) = %q, which still contains the password", tc.dsn, got)
			}
			for _, keep := range tc.keep {
				if !strings.Contains(got, keep) {
					t.Errorf("RedactDSN(%q) = %q, which lost %q", tc.dsn, got, keep)
				}
			}
		})
	}
}

func TestShouldRedactHeader(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Authorization", "authorization", "COOKIE", "Set-Cookie", "X-Api-Key", "Proxy-Authorization"} {
		if !config.ShouldRedactHeader(name, nil) {
			t.Errorf("%s should always be redacted", name)
		}
	}
	if config.ShouldRedactHeader("Accept", nil) {
		t.Error("Accept is not a secret")
	}
	if !config.ShouldRedactHeader("X-Internal-Token", []string{"x-internal-token"}) {
		t.Error("a configured header was not redacted")
	}
}

func TestApplyPolicy(t *testing.T) {
	t.Parallel()

	writeConfig := func(t *testing.T, extra string) *config.Config {
		t.Helper()
		return mustLoad(t, `
version: 1
run: { duration: 10s }
`+extra, nil)
	}

	t.Run("writes are refused without a grant", func(t *testing.T) {
		t.Parallel()
		cfg := writeConfig(t, `
db:
  driver: postgres
  dsn: "postgres://app@10.0.0.5/shop"
  executor: { rate: 1 }
  queries: [{ name: insert-item, sql: "INSERT INTO items (t) VALUES ($1)" }]
`)
		_, _, err := cfg.ApplyPolicy(policy.Default())
		if err == nil {
			t.Fatal("a write was allowed without allow_writes")
		}
		var typed *errs.Error
		if !errors.As(err, &typed) || typed.Code != errs.CodePolicyWritesNotAllowed {
			t.Fatalf("code = %v, want %s", err, errs.CodePolicyWritesNotAllowed)
		}
		if !strings.Contains(typed.Message, "insert-item") {
			t.Errorf("message = %q, want it to name the query", typed.Message)
		}
	})

	t.Run("writes are allowed when both grant them", func(t *testing.T) {
		t.Parallel()
		cfg := writeConfig(t, `
safety: { allow_writes: true }
db:
  driver: postgres
  dsn: "postgres://app@10.0.0.5/shop"
  executor: { rate: 1 }
  queries: [{ name: insert-item, sql: "INSERT INTO items (t) VALUES ($1)" }]
`)
		granted := policy.Default()
		granted.AllowWrites = true
		if _, _, err := cfg.ApplyPolicy(granted); err != nil {
			t.Fatalf("ApplyPolicy: %v", err)
		}
	})

	t.Run("a destructive statement needs more than allow_writes", func(t *testing.T) {
		t.Parallel()
		cfg := writeConfig(t, `
safety: { allow_writes: true }
db:
  driver: postgres
  dsn: "postgres://app@10.0.0.5/shop"
  executor: { rate: 1 }
  queries: [{ name: reset, type: write, sql: "TRUNCATE items" }]
`)
		granted := policy.Default()
		granted.AllowWrites = true
		_, _, err := cfg.ApplyPolicy(granted)
		if err == nil {
			t.Fatal("TRUNCATE was allowed on an allow_writes grant alone")
		}
		var typed *errs.Error
		if !errors.As(err, &typed) || typed.Code != errs.CodePolicyDangerousNotAllowed {
			t.Fatalf("code = %v, want %s", err, errs.CodePolicyDangerousNotAllowed)
		}
	})

	t.Run("a redis write is caught by the classifier", func(t *testing.T) {
		t.Parallel()
		cfg := writeConfig(t, `
redis:
  addr: "10.0.0.6:6379"
  executor: { rate: 1 }
  commands: [{ name: put, cmd: [SET, "k", "v"] }]
`)
		if _, _, err := cfg.ApplyPolicy(policy.Default()); err == nil {
			t.Fatal("a Redis SET was allowed without allow_writes")
		}
	})

	t.Run("an O(N) blocker is warned about, not refused", func(t *testing.T) {
		t.Parallel()
		cfg := writeConfig(t, `
redis:
  addr: "10.0.0.6:6379"
  executor: { rate: 1 }
  commands: [{ name: scan-all, cmd: [KEYS, "*"] }]
`)
		_, warnings, err := cfg.ApplyPolicy(policy.Default())
		if err != nil {
			t.Fatalf("KEYS should be allowed: %v", err)
		}
		var found bool
		for _, w := range warnings {
			if w.Code == "DANGEROUS_COMMAND" && strings.Contains(w.Message, "KEYS") {
				found = true
			}
		}
		if !found {
			t.Errorf("KEYS produced no warning: %+v", warnings)
		}
	})

	t.Run("a declared read that looks like a write is flagged", func(t *testing.T) {
		t.Parallel()
		cfg := writeConfig(t, `
db:
  driver: postgres
  dsn: "postgres://app@10.0.0.5/shop"
  executor: { rate: 1 }
  queries: [{ name: sneaky, type: read, sql: "DELETE FROM items" }]
`)
		_, warnings, err := cfg.ApplyPolicy(policy.Default())
		if err != nil {
			t.Fatalf("a declared read is authoritative and should pass: %v", err)
		}
		var found bool
		for _, w := range warnings {
			if w.Code == "DECLARED_READ_LOOKS_LIKE_WRITE" {
				found = true
			}
		}
		if !found {
			t.Errorf("a declared read containing DELETE was not flagged: %+v", warnings)
		}
	})

	t.Run("the peak rate is what the ceiling applies to", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, `
version: 1
run: { duration: 40s }
http:
  executor:
    stages: [{ duration: 10s, target: 900 }, { duration: 30s, target: 10 }]
  requests: [{ name: probe, url: "http://127.0.0.1/" }]
`, nil)
		granted := policy.Default()
		granted.MaxRatePerRunner = ptr(100.0)
		_, _, err := cfg.ApplyPolicy(granted)
		if err == nil {
			t.Fatal("a profile peaking at 900/s was allowed under a 100/s ceiling")
		}
		var typed *errs.Error
		if !errors.As(err, &typed) || typed.Code != errs.CodePolicyRateExceeded {
			t.Errorf("code = %v, want %s", err, errs.CodePolicyRateExceeded)
		}
	})

	t.Run("duration and concurrency ceilings", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoad(t, `
version: 1
run: { duration: 30m }
http:
  executor: { rate: 10, max_in_flight: 5000 }
  requests: [{ name: probe, url: "http://127.0.0.1/" }]
`, nil)
		granted := policy.ServerDefault()
		_, _, err := cfg.ApplyPolicy(granted)
		if err == nil {
			t.Fatal("a 30-minute run with 5000 in flight was allowed under the server envelope")
		}
		var typed *errs.Error
		if !errors.As(err, &typed) {
			t.Fatalf("not coded: %v", err)
		}
		// Both ceilings are exceeded, so both should be reported in one pass.
		if len(typed.Causes) < 2 {
			t.Errorf("reported %d problems, want both the duration and the concurrency", len(typed.Causes))
		}
	})

	t.Run("insecure tls is refused without a grant", func(t *testing.T) {
		t.Parallel()
		cfg := writeConfig(t, `
http:
  executor: { rate: 1 }
  transport: { tls: { insecure_skip_verify: true } }
  requests: [{ name: probe, url: "https://10.0.0.9/" }]
`)
		if _, _, err := cfg.ApplyPolicy(policy.Default()); err == nil {
			t.Fatal("insecure TLS was allowed without a grant")
		}
	})
}

func TestTargetsAreDiscoveredForPreflight(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, `
version: 1
run: { duration: 10s }
http:
  base_url: "http://app.internal:8080"
  executor: { rate: 1 }
  requests:
    - { name: a, url: "/api/a" }
    - { name: b, url: "http://other.internal/b" }
db:
  driver: postgres
  dsn: "postgres://app:pw@db.internal:5432/shop"
  executor: { rate: 1 }
  queries: [{ name: q, sql: "SELECT 1" }]
redis:
  addr: "cache.internal:6379"
  executor: { rate: 1 }
  commands: [{ name: c, cmd: [GET, k] }]
`, nil)

	got := cfg.Targets()
	want := map[string]bool{"app.internal": true, "other.internal": true, "db.internal": true, "cache.internal": true}
	if len(got) != len(want) {
		t.Fatalf("Targets() = %v, want %d hosts", got, len(want))
	}
	for _, h := range got {
		if !want[h] {
			t.Errorf("unexpected target %q", h)
		}
	}
}

func TestHostExtractionFromDSNShapes(t *testing.T) {
	t.Parallel()
	cases := []struct{ driver, dsn, want string }{
		{"postgres", "postgres://app:pw@db.internal:5432/shop", "db.internal"},
		{"postgres", "host=db.internal port=5432 user=app", "db.internal"},
		{"mysql", "app:pw@tcp(db.internal:3306)/shop", "db.internal"},
		{"sqlite", "file:test.db", ""},
	}
	for _, tc := range cases {
		t.Run(tc.driver+"/"+tc.dsn[:min(len(tc.dsn), 20)], func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Version: 1,
				DB:      &config.DB{Driver: tc.driver, DSN: tc.dsn},
			}
			got := cfg.Targets()
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("Targets() = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("Targets() = %v, want [%s]", got, tc.want)
			}
		})
	}
}
