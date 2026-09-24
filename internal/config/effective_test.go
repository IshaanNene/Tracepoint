package config_test

import (
	"context"
	"strings"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/config"
)

func TestEffectiveYAMLKeepsReferencesAndRedactsInlineSecrets(t *testing.T) {
	raw := []byte(`
version: 1
run: { duration: 10s }
http:
  base_url: "${BASE_URL}"
  headers: { Authorization: "Bearer inline-secret" }
  executor: { rate: 5 }
  requests: [{ name: a, url: /x }]
db:
  driver: postgres
  dsn: "postgres://u:pw@db:5432/app"
  executor: { rate: 5 }
  queries: [{ name: q, sql: "SELECT 1" }]
redis:
  addr: "${REDIS_ADDR:-127.0.0.1:6379}"
  executor: { rate: 5 }
  commands: [{ name: c, cmd: [PING] }]
`)
	lookup := func(k string) (string, bool) {
		return map[string]string{"BASE_URL": "http://resolved"}[k], k == "BASE_URL"
	}
	doc, preserved, err := config.EffectiveYAML(context.Background(), raw, []string{"http.executor.rate=9"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if !preserved {
		t.Fatalf("references should have been preserved:\n%s", s)
	}
	for _, want := range []string{"${BASE_URL}", "${REDIS_ADDR:-127.0.0.1:6379}", "rate: 9", "Overrides: http.executor.rate=9", config.Redacted} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	for _, leak := range []string{"inline-secret", ":pw@", "http://resolved"} {
		if strings.Contains(s, leak) {
			t.Errorf("%q leaked into:\n%s", leak, s)
		}
	}
	// The defaults were not written, so the shorthand survives and reloads cleanly.
	if strings.Contains(s, "stages") {
		t.Errorf("defaults were written:\n%s", s)
	}
	env := func(k string) (string, bool) {
		v, ok := map[string]string{"BASE_URL": "http://h", "REDIS_ADDR": "r:1"}[k]
		return v, ok
	}
	cfg, err := config.Load(context.Background(), config.FromBytes("x", doc), config.Options{Lookup: env})
	if err != nil {
		t.Fatalf("the stored file does not load: %v", err)
	}
	if got := cfg.RedactedPaths(); len(got) != 2 {
		t.Fatalf("redacted paths = %v, want the header and the dsn", got)
	}
}

// A reference where a number belongs cannot be kept, so values are resolved and the
// caller is told.
func TestEffectiveYAMLFallsBackWhenAReferenceIsTyped(t *testing.T) {
	raw := []byte("version: 1\nrun: { duration: 10s }\nhttp:\n  executor: { rate: ${RATE} }\n  requests: [{ name: a, url: \"http://h/\" }]\n")
	doc, preserved, err := config.EffectiveYAML(context.Background(), raw, nil,
		func(string) (string, bool) { return "7", true })
	if err != nil {
		t.Fatal(err)
	}
	if preserved || !strings.Contains(string(doc), "rate: 7") || !strings.Contains(string(doc), "values here are") {
		t.Fatalf("preserved=%v:\n%s", preserved, doc)
	}
}

// A header is addressed by its key, including one the configuration did not have -
// which is how a secret a stored configuration redacted is supplied again.
func TestSetAddressesMapKeys(t *testing.T) {
	cfg, err := config.Load(context.Background(), config.FromBytes("x", []byte(
		"version: 1\nrun: { duration: 10s }\nhttp:\n  headers: { A: one }\n  executor: { rate: 1 }\n  requests: [{ name: r, url: \"http://h/\" }]\n")),
		config.Options{Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplySet([]string{"http.headers.A=two", "http.headers.Authorization=Bearer x",
		"http.requests[r].headers.X-Trace=1"}); err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Headers["A"] != "two" || cfg.HTTP.Headers["Authorization"] != "Bearer x" ||
		cfg.HTTP.Requests[0].Headers["X-Trace"] != "1" {
		t.Fatalf("headers = %v / %v", cfg.HTTP.Headers, cfg.HTTP.Requests[0].Headers)
	}
	if err := cfg.ApplySet([]string{"http.headers.A.b=1"}); err == nil {
		t.Fatal("a map value has no fields")
	}
}
