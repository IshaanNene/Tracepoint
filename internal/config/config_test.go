package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func noEnv(string) (string, bool) { return "", false }

func env(pairs map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := pairs[k]; return v, ok }
}

func load(t *testing.T, doc string, lookup func(string) (string, bool)) (*config.Config, error) {
	t.Helper()
	if lookup == nil {
		lookup = noEnv
	}
	return config.Load(t.Context(), config.FromBytes("test.yaml", []byte(doc)), config.Options{Lookup: lookup})
}

func mustLoad(t *testing.T, doc string, lookup func(string) (string, bool)) *config.Config {
	t.Helper()
	cfg, err := load(t, doc, lookup)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

const minimal = `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 5 }
  requests:
    - { name: probe, url: "http://127.0.0.1:8080/" }
`

func TestLoadMinimal(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, minimal, nil)

	if cfg.Version != 1 {
		t.Errorf("version = %d, want 1", cfg.Version)
	}
	if got, want := cfg.Run.Duration.D(), 10*time.Second; got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
	// Defaults fill in everything the document left unsaid.
	if got, want := cfg.Run.Bucket.D(), config.DefaultBucket; got != want {
		t.Errorf("bucket = %v, want the default %v", got, want)
	}
	if cfg.Run.Arrival != "uniform" {
		t.Errorf("arrival = %q, want the default uniform", cfg.Run.Arrival)
	}
	if cfg.Run.MinSamples != config.DefaultMinSamples {
		t.Errorf("min_samples = %d, want %d", cfg.Run.MinSamples, config.DefaultMinSamples)
	}
	if got := cfg.HTTP.Requests[0].Method; got != "GET" {
		t.Errorf("method = %q, want the default GET", got)
	}
	if got := cfg.HTTP.Requests[0].WeightOr(); got != 1 {
		t.Errorf("weight = %v, want the default 1", got)
	}
}

// `rate:` is shorthand for a constant rate held for the whole run, not a ramp up to
// it. Getting this wrong would halve the load of every simple configuration.
func TestRateShorthandIsConstantNotARamp(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, minimal, nil)
	e := cfg.HTTP.Executor

	if len(e.Stages) != 1 {
		t.Fatalf("got %d stages, want the shorthand expanded to 1", len(e.Stages))
	}
	if got, want := e.Stages[0].Duration.D(), 10*time.Second; got != want {
		t.Errorf("stage duration = %v, want the whole run %v", got, want)
	}
	if e.Stages[0].Target != 5 {
		t.Errorf("stage target = %v, want 5", e.Stages[0].Target)
	}
	if got := e.StartRate(); got != 5 {
		t.Errorf("StartRate() = %v, want 5; the shorthand must hold its rate from the first instant", got)
	}
}

// An explicit stages list ramps up from nothing, which is the opposite of the
// shorthand and is what the spec's own example relies on.
func TestExplicitStagesRampFromZero(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, `
version: 1
run: { duration: 40s }
http:
  executor:
    stages: [{ duration: 10s, target: 100 }, { duration: 30s, target: 100 }]
  requests: [{ name: probe, url: "http://127.0.0.1/" }]
`, nil)
	if got := cfg.HTTP.Executor.StartRate(); got != 0 {
		t.Errorf("StartRate() = %v, want 0; an explicit stage list ramps up from nothing", got)
	}
}

func TestUnknownFieldReportsPositionAndSuggestion(t *testing.T) {
	t.Parallel()
	_, err := load(t, `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 5 }
  requests:
    - name: probe
      metod: GET
      url: "http://127.0.0.1/"
`, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) {
		t.Fatalf("not a coded error: %v", err)
	}
	if typed.Code != errs.CodeConfigUnknownField {
		t.Errorf("code = %s, want %s", typed.Code, errs.CodeConfigUnknownField)
	}
	if !strings.Contains(typed.Hint, "method") {
		t.Errorf("hint = %q, want a suggestion of \"method\"", typed.Hint)
	}
	if typed.Line != 8 {
		t.Errorf("line = %d, want 8", typed.Line)
	}
	if typed.Column == 0 {
		t.Error("an unknown field should carry a column")
	}
	if typed.Path != "/http/requests/0/metod" {
		t.Errorf("path = %q, want /http/requests/0/metod", typed.Path)
	}
}

// Three typos should be reported as three problems. A load test is slow enough that
// finding out about the second mistake on the second run is a real cost.
func TestEveryUnknownFieldIsReportedAtOnce(t *testing.T) {
	t.Parallel()
	_, err := load(t, `
version: 1
run: { duration: 10s, buckets: 1s }
http:
  executer: { rate: 5 }
  requests:
    - { name: probe, url: "http://127.0.0.1/", methd: GET }
`, nil)
	if err == nil {
		t.Fatal("expected errors")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) {
		t.Fatalf("not a coded error: %v", err)
	}
	if len(typed.Causes) != 3 {
		t.Fatalf("reported %d problems, want 3 (buckets, executer, methd): %v", len(typed.Causes), typed.Message)
	}
	paths := map[string]bool{}
	for _, c := range typed.Causes {
		paths[c.Path] = true
	}
	for _, want := range []string{"/run/buckets", "/http/executer", "/http/requests/0/methd"} {
		if !paths[want] {
			t.Errorf("missing a problem for %s; got %v", want, paths)
		}
	}
}

func TestEnvInterpolation(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 5 }
  requests:
    - { name: probe, url: "${BASE_URL}/api/items" }
    - { name: other, url: "${MISSING:-http://fallback.internal}/x" }
`, env(map[string]string{"BASE_URL": "http://127.0.0.1:9999"}))

	if got, want := cfg.HTTP.Requests[0].URL, "http://127.0.0.1:9999/api/items"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if got, want := cfg.HTTP.Requests[1].URL, "http://fallback.internal/x"; got != want {
		t.Errorf("defaulted url = %q, want %q", got, want)
	}
}

// An unset variable with no default is an error, never an empty string: substituting
// nothing produces a configuration that parses, runs, and measures the wrong thing.
func TestUnsetEnvIsAnErrorNotAnEmptyString(t *testing.T) {
	t.Parallel()
	_, err := load(t, `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 5 }
  requests: [{ name: probe, url: "${NOT_SET_ANYWHERE}/api" }]
`, noEnv)
	if err == nil {
		t.Fatal("expected an error for an unset variable")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodeConfigEnvUnset {
		t.Fatalf("code = %v, want %s", err, errs.CodeConfigEnvUnset)
	}
	if !strings.Contains(typed.Hint, "NOT_SET_ANYWHERE") {
		t.Errorf("hint = %q, want it to name the variable", typed.Hint)
	}
}

func TestValidationRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		code errs.Code
	}{
		{"no version", "run: { duration: 10s }\nhttp: { executor: { rate: 1 }, requests: [{name: a, url: u}] }", errs.CodeConfigVersionUnsupported},
		{"wrong version", "version: 2\nhttp: { executor: { rate: 1 }, requests: [{name: a, url: u}] }", errs.CodeConfigVersionUnsupported},
		{"no runner", "version: 1\nrun: { duration: 10s }", errs.CodeConfigNoRunner},
		{
			"requests and journeys",
			"version: 1\nrun: {duration: 10s}\nhttp:\n  executor: {rate: 1}\n  requests: [{name: a, url: u}]\n  journeys: [{name: j, steps: [{name: s, url: u}]}]",
			errs.CodeConfigRequestsAndJourney,
		},
		{"zero weight", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u, weight: 0}]}", errs.CodeConfigInvalidValue},
		{"negative weight", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u, weight: -2}]}", errs.CodeConfigInvalidValue},
		{"duplicate names", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u}, {name: a, url: v}]}", errs.CodeConfigDuplicateName},
		{"no url", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {rate: 1}, requests: [{name: a}]}", errs.CodeConfigMissingField},
		{"no name", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {rate: 1}, requests: [{url: u}]}", errs.CodeConfigMissingField},
		{"empty executor", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {}, requests: [{name: a, url: u}]}", errs.CodeConfigMissingField},
		{"vus executor given a rate", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {type: vus, rate: 5}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"unknown executor", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {type: burst, rate: 5}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"bad arrival", "version: 1\nrun: {duration: 10s, arrival: gaussian}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"warmup covers the run", "version: 1\nrun: {duration: 10s, warmup: 20s}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"bucket longer than the run", "version: 1\nrun: {duration: 2s, bucket: 10s}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"error rate above one", "version: 1\nrun: {duration: 10s}\nslo: {http: {error_rate: 2}}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"p95 looser than p99", "version: 1\nrun: {duration: 10s}\nslo: {http: {p95: 500ms, p99: 100ms}}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"bad duration", "version: 1\nrun: {duration: 3 minutes}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u}]}", errs.CodeConfigInvalidValue},
		{"body and body_file", "version: 1\nrun: {duration: 10s}\nhttp: {executor: {rate: 1}, requests: [{name: a, url: u, body: x, body_file: f}]}", errs.CodeConfigInvalidValue},
		{"unknown driver", "version: 1\nrun: {duration: 10s}\ndb: {driver: oracle, dsn: d, executor: {rate: 1}, queries: [{name: q, sql: S}]}", errs.CodeConfigInvalidValue},
		{"query with neither sql nor tx", "version: 1\nrun: {duration: 10s}\ndb: {driver: postgres, dsn: d, executor: {rate: 1}, queries: [{name: q}]}", errs.CodeConfigMissingField},
		{"redis addr and addrs", "version: 1\nrun: {duration: 10s}\nredis: {addr: a, addrs: [b], executor: {rate: 1}, commands: [{name: c, cmd: [GET, k]}]}", errs.CodeConfigInvalidValue},
		{"empty document", "", errs.CodeConfigParse},
		{"not yaml", "version: 1\n\tbad indentation: [", errs.CodeConfigParse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := load(t, tc.doc, nil)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			var typed *errs.Error
			if !errors.As(err, &typed) {
				t.Fatalf("not a coded error: %v", err)
			}
			got := typed.Code
			if got != tc.code {
				// A summary error carries the real codes in its causes.
				for _, c := range typed.Causes {
					if c.Code == tc.code {
						return
					}
				}
				t.Errorf("code = %s, want %s (%v)", got, tc.code, typed.Message)
			}
		})
	}
}

// Stages define the shape of the load and run.duration says how long to measure. If
// they disagree the run would stop mid-ramp or idle at the end, and either way the
// result is not the test that was written.
func TestStageDurationsMustSumToTheRunDuration(t *testing.T) {
	t.Parallel()
	_, err := load(t, `
version: 1
run: { duration: 60s }
http:
  executor:
    stages: [{ duration: 10s, target: 100 }, { duration: 20s, target: 100 }]
  requests: [{ name: a, url: u }]
`, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodeConfigStagesMismatch {
		t.Fatalf("code = %v, want %s", err, errs.CodeConfigStagesMismatch)
	}
}

func TestJourneyStepsCannotCarryWeights(t *testing.T) {
	t.Parallel()
	_, err := load(t, `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 1 }
  journeys:
    - name: j
      steps: [{ name: s, url: u, weight: 3 }]
`, nil)
	if err == nil {
		t.Fatal("a journey step carrying a weight should be rejected: steps run in order")
	}
}

func TestTelemetryAcceptsBothForms(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, `
version: 1
run: { duration: 10s }
http: { executor: { rate: 1 }, requests: [{ name: a, url: u }] }
telemetry:
  postgres: true
  redis: { enabled: true, interval: 2s, latency: true }
`, nil)

	if cfg.Telemetry.Postgres == nil || !cfg.Telemetry.Postgres.Enabled {
		t.Error("the boolean shorthand did not enable the postgres sampler")
	}
	if cfg.Telemetry.Redis == nil || !cfg.Telemetry.Redis.Enabled || !cfg.Telemetry.Redis.Latency {
		t.Errorf("the object form did not configure the redis sampler: %+v", cfg.Telemetry.Redis)
	}
	if got := cfg.Telemetry.Redis.Interval.D(); got != 2*time.Second {
		t.Errorf("interval = %v, want 2s", got)
	}
}

func TestDriverAliasesNormalise(t *testing.T) {
	t.Parallel()
	for _, alias := range []string{"postgresql", "psql", "pg", "postgres"} {
		if got := config.NormaliseDriver(alias); got != "postgres" {
			t.Errorf("NormaliseDriver(%q) = %q, want postgres", alias, got)
		}
	}
	if got := config.NormaliseDriver("mariadb"); got != "mysql" {
		t.Errorf("NormaliseDriver(mariadb) = %q, want mysql", got)
	}
}

func TestLabelsAndRunners(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, `
version: 1
run: { duration: 10s }
http:
  executor: { rate: 1 }
  journeys:
    - name: buy
      steps: [{ name: list, url: u }, { name: order, url: v }]
db:
  driver: postgres
  dsn: d
  executor: { rate: 1 }
  queries: [{ name: by-id, sql: "SELECT 1" }]
`, nil)

	if got, want := cfg.Runners(), []string{"http", "db"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Runners() = %v, want %v", got, want)
	}
	// A journey's steps are labelled journey/step so two journeys can share a step
	// name without their statistics merging.
	labels := cfg.Labels("http")
	if len(labels) != 2 || labels[0] != "buy/list" || labels[1] != "buy/order" {
		t.Errorf("http labels = %v, want [buy/list buy/order]", labels)
	}
	if got := cfg.Labels("db"); len(got) != 1 || got[0] != "by-id" {
		t.Errorf("db labels = %v, want [by-id]", got)
	}
	if config.Kind("http") != "app" || config.Kind("db") != "storage" {
		t.Error("http must be the app tier and db a storage tier")
	}
}

func TestLoadFromFileAndMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "tracepoint.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	if _, err := config.Load(t.Context(), config.FromFile(path), config.Options{Lookup: noEnv}); err != nil {
		t.Errorf("loading from a file: %v", err)
	}

	_, err := config.Load(t.Context(), config.FromFile(filepath.Join(dir, "absent.yaml")), config.Options{Lookup: noEnv})
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodeConfigNotFound {
		t.Errorf("missing file: got %v, want %s", err, errs.CodeConfigNotFound)
	}
}

func TestLoadFromStdin(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(t.Context(), config.FromFile("-"), config.Options{
		Lookup: noEnv,
		Stdin:  strings.NewReader(minimal),
	})
	if err != nil {
		t.Fatalf("loading from stdin: %v", err)
	}
	if len(cfg.HTTP.Requests) != 1 {
		t.Errorf("got %d requests from stdin", len(cfg.HTTP.Requests))
	}
}

// The schema and these structs are two halves of one contract. Running the schema's
// own fixtures through the loader is what keeps them from drifting: a field renamed in
// one and not the other fails here.
func TestSchemaFixturesRoundTrip(t *testing.T) {
	t.Parallel()
	lookup := env(map[string]string{
		"BASE_URL": "http://127.0.0.1:8080", "PG_DSN": "postgres://u:p@127.0.0.1/db",
		"MY_DSN": "u:p@tcp(127.0.0.1)/db", "REDIS_ADDR": "127.0.0.1:6379",
		"REDIS_PASSWORD": "secret",
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		files, err := filepath.Glob("../schemas/testdata/config/valid/*.json")
		if err != nil || len(files) == 0 {
			t.Fatalf("no valid fixtures found: %v", err)
		}
		for _, f := range files {
			name := filepath.Base(f)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				data, err := os.ReadFile(f)
				if err != nil {
					t.Fatalf("reading: %v", err)
				}
				// These fixtures describe shapes, not runnable tests: several use
				// executor shorthands whose stage sums cannot match an unset duration,
				// so defaults are applied but semantics are checked separately below.
				if _, err := config.Load(t.Context(), config.FromBytes(name, data), config.Options{Lookup: lookup}); err != nil {
					t.Errorf("the schema accepts this fixture but the loader rejects it:\n%v", err)
				}
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		files, err := filepath.Glob("../schemas/testdata/config/invalid/*.json")
		if err != nil || len(files) == 0 {
			t.Fatalf("no invalid fixtures found: %v", err)
		}
		for _, f := range files {
			name := filepath.Base(f)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				data, err := os.ReadFile(f)
				if err != nil {
					t.Fatalf("reading: %v", err)
				}
				if _, err := config.Load(t.Context(), config.FromBytes(name, data), config.Options{Lookup: lookup}); err == nil {
					t.Error("the schema rejects this fixture but the loader accepts it")
				}
			})
		}
	})
}
