package result_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/analysis"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/result/resulttest"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var update = flag.Bool("update", false, "rewrite the golden files")

func encode(t *testing.T, d *result.Digest) []byte {
	t.Helper()
	raw, err := d.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return raw
}

func recIDs(d *result.Digest) []string {
	out := make([]string, 0, len(d.Recommendations))
	for _, r := range d.Recommendations {
		out = append(out, r.ID)
	}
	return out
}

func rec(d *result.Digest, id string) *result.Recommendation {
	for i := range d.Recommendations {
		if d.Recommendations[i].ID == id {
			return &d.Recommendations[i]
		}
	}
	return nil
}

func dbIncidentRun() *result.Result {
	b := resulttest.NewRun(90).Fault("db", 30, 34, 900).Fault("http", 31, 35, 950)
	var samples []map[string]any
	for i := range 90 {
		v := 0.0
		if i >= 30 && i <= 34 {
			v = 14
		}
		samples = append(samples, map[string]any{"t_ms": float64(i * 1000), "locks_waiting": v, "sample_ms": 0.8})
	}
	b.R.Telemetry = &result.Telemetry{
		Postgres:  &result.SamplerSeries{Available: true, IntervalMS: 1000, Samples: samples},
		Generator: &result.GeneratorSeries{Samples: []map[string]any{{"t_ms": 0.0, "cpu_ratio": 0.12, "sched_latency_p99_ms": 0.05}}},
	}
	b.R.Artifacts = &result.Artifacts{RunDir: "runs/20260924T100000Z-abc123", Result: "runs/20260924T100000Z-abc123/result.json"}
	budget := config.Duration(250 * time.Millisecond)
	return b.Analysed(analysis.Inputs{
		SLO:       config.SLO{HTTP: config.SLOTarget{P99: &budget}},
		Telemetry: []string{"postgres"},
	})
}

func TestDigestSatisfiesSchema(t *testing.T) {
	d := result.BuildDigest(dbIncidentRun(), result.DigestOptions{BudgetChars: -1})
	schematest.Validate(t, schemas.Digest, encode(t, d))
	if d.Truncated || d.More != nil {
		t.Fatalf("an unlimited digest is never truncated")
	}
	if d.Verdict.Bottleneck != "db" || len(d.Incidents) != 1 || d.Incidents[0].Culprit == nil || *d.Incidents[0].Culprit != "db" {
		t.Fatalf("digest = %+v", d)
	}
	if got := d.Incidents[0].Corroboration; len(got) != 1 || got[0] != "postgres locks_waiting moved from 0 to 14" {
		t.Fatalf("corroboration = %v", got)
	}
}

func TestDigestGolden(t *testing.T) {
	raw := encode(t, result.BuildDigest(dbIncidentRun(), result.DigestOptions{BudgetChars: -1}))
	path := filepath.Join("testdata", "digest-db.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\nrun `go test ./internal/result/ -update` to create it", path, err)
	}
	if !bytes.Equal(want, raw) {
		t.Errorf("digest differs from %s\n--- want ---\n%s\n--- got ---\n%s", path, want, raw)
	}
}

// Over budget, the lowest-priority content goes first, and what went is named.
func TestDigestBudget(t *testing.T) {
	r := dbIncidentRun()
	full := encode(t, result.BuildDigest(r, result.DigestOptions{BudgetChars: -1}))

	for _, budget := range []int{3000, 2000, 1500} {
		d := result.BuildDigest(r, result.DigestOptions{BudgetChars: budget, Ref: "runs/x/result.json"})
		if len(full) <= budget {
			t.Fatalf("the fixture must exceed %d to test truncation; it is %d", budget, len(full))
		}
		if !d.Truncated || d.More == nil || len(d.More.Dropped) == 0 {
			t.Fatalf("budget %d: not marked truncated: %+v", budget, d.More)
		}
		// The budget holds for what is actually written, not some other encoding.
		if got := len(encode(t, d)); got > budget {
			t.Errorf("budget %d: %d chars still over", budget, got)
		}
		if !strings.Contains(d.More.Hint, "tracepoint digest runs/x/result.json --budget-chars") {
			t.Errorf("hint = %q", d.More.Hint)
		}
		// Validity and the verdict are never dropped.
		if d.Validity.State == "" || d.Verdict.Bottleneck != "db" || d.Verdict.Summary == "" {
			t.Fatalf("budget %d dropped the essentials: %+v", budget, d)
		}
		schematest.Validate(t, schemas.Digest, encode(t, d))
	}

	// A budget below the required core is honoured as far as it can be, and the core
	// itself - validity, SLO, verdict - survives.
	d := result.BuildDigest(r, result.DigestOptions{BudgetChars: 200})
	if !d.Truncated || d.Verdict.Summary == "" || d.Verdict.Caveat == "" {
		t.Fatalf("the core was dropped: %+v", d)
	}
	schematest.Validate(t, schemas.Digest, encode(t, d))

	// Dropping is ordered: telemetry goes before incidents do.
	d = result.BuildDigest(r, result.DigestOptions{BudgetChars: 2500})
	if len(d.More.Dropped) == 0 || d.More.Dropped[0] != "telemetry" {
		t.Fatalf("dropped = %v, want telemetry first", d.More.Dropped)
	}
}

func TestDigestDefaultBudget(t *testing.T) {
	d := result.BuildDigest(dbIncidentRun(), result.DigestOptions{})
	if raw := encode(t, d); len(raw) > result.DefaultDigestBudget {
		t.Fatalf("default digest is %d chars", len(raw))
	}
}

func TestRecommendClientCapped(t *testing.T) {
	b := resulttest.NewRun(60)
	http := b.R.RunnerByName("http")
	for i := range http.Buckets {
		http.Buckets[i].Offered = 120
		if i >= 20 {
			http.Buckets[i].Dropped = 30
		}
	}
	http.Executor.Offered, http.Executor.Dropped = 7200, 1200
	http.Executor.DroppedRatio = 1200.0 / 7200
	r := b.Analysed(analysis.Inputs{})
	if r.Analysis.Validity.State != result.ValidityInvalid {
		t.Fatalf("validity = %s", r.Analysis.Validity.State)
	}
	d := result.BuildDigest(r, result.DigestOptions{BudgetChars: -1})
	got := rec(d, "raise-max-in-flight-http")
	if got == nil || got.Action != result.ActionRerun || got.Priority != 1 {
		t.Fatalf("recommendations = %+v", d.Recommendations)
	}
	// 120/s offered x 20ms p99 x 1.2 = 2.9, below the 4 configured, so double it.
	if len(got.Overrides) != 1 || got.Overrides[0] != "http.executor.max_in_flight=8" {
		t.Fatalf("overrides = %v", got.Overrides)
	}
}

func TestRecommendEnableTelemetry(t *testing.T) {
	r := resulttest.NewRun(90).Fault("db", 30, 34, 900).Fault("http", 31, 35, 950).Analysed(analysis.Inputs{})
	d := result.BuildDigest(r, result.DigestOptions{BudgetChars: -1})
	got := rec(d, "enable-telemetry-postgres")
	if got == nil || got.Overrides[0] != "telemetry.postgres.enabled=true" {
		t.Fatalf("recommendations = %v", recIDs(d))
	}
	if inv := rec(d, "investigate-db"); inv == nil || !strings.Contains(inv.Why, "30s-36s") {
		t.Fatalf("investigate-db = %+v", inv)
	}
}

func TestRecommendUnobserved(t *testing.T) {
	b := resulttest.NewRun(90).Fault("http", 30, 34, 900)
	db := b.R.RunnerByName("db")
	for i := 28; i <= 36; i++ {
		db.Buckets[i].Insufficient = true
		db.Buckets[i].N = 3
	}
	r := b.Analysed(analysis.Inputs{})
	d := result.BuildDigest(r, result.DigestOptions{BudgetChars: -1})
	got := rec(d, "raise-probe-rate-db")
	// 20 samples per 1s bucket with half again to spare: 30/s, above the 10 configured.
	if got == nil || got.Overrides[0] != "db.executor.rate=30" {
		t.Fatalf("recommendations = %+v", d.Recommendations)
	}

	b = resulttest.NewRun(90).Fault("http", 30, 34, 900)
	b.R.Runners = b.R.Runners[:1]
	d = result.BuildDigest(b.Analysed(analysis.Inputs{}), result.DigestOptions{BudgetChars: -1})
	if rec(d, "add-storage-probes") == nil {
		t.Fatalf("recommendations = %v", recIDs(d))
	}
}

func TestRecommendQuietRun(t *testing.T) {
	d := result.BuildDigest(resulttest.NewRun(60).Analysed(analysis.Inputs{}), result.DigestOptions{BudgetChars: -1})
	if rec(d, "configure-slo") == nil {
		t.Fatalf("recommendations = %v", recIDs(d))
	}
	if got := rec(d, "raise-load"); got == nil || got.Overrides[0] != "http.executor.rate=200" {
		t.Fatalf("raise-load = %+v", got)
	}
}

// A staged profile has no single rate to override, so the advice is a configuration
// change, never an override that would not apply.
func TestRecommendStagedProfile(t *testing.T) {
	b := resulttest.NewRun(60)
	b.R.Config.Effective = map[string]any{"http": map[string]any{"executor": map[string]any{"stages": []any{}}}}
	d := result.BuildDigest(b.Analysed(analysis.Inputs{}), result.DigestOptions{BudgetChars: -1})
	got := rec(d, "raise-load")
	if got == nil || got.Action != result.ActionConfigure || len(got.Overrides) != 0 {
		t.Fatalf("raise-load = %+v", got)
	}
}

func TestRecommendGrantTelemetry(t *testing.T) {
	b := resulttest.NewRun(30)
	b.R.Telemetry = &result.Telemetry{Postgres: &result.SamplerSeries{Available: false, Reason: "permission denied"}}
	d := result.BuildDigest(b.Analysed(analysis.Inputs{Telemetry: []string{"postgres"}}), result.DigestOptions{BudgetChars: -1})
	got := rec(d, "grant-telemetry-postgres")
	if got == nil || !strings.Contains(got.Why, "pg_read_all_stats") {
		t.Fatalf("recommendations = %+v", d.Recommendations)
	}
	if len(d.Telemetry) == 0 || d.Telemetry[0].Available || d.Telemetry[0].Reason != "permission denied" {
		t.Fatalf("telemetry = %+v", d.Telemetry)
	}
}

func TestDigestTieNamesNoCulprit(t *testing.T) {
	r := resulttest.NewRun(90).Fault("db", 30, 33, 400).Fault("redis", 30, 33, 400).Fault("http", 30, 33, 900).Analysed(analysis.Inputs{})
	d := result.BuildDigest(r, result.DigestOptions{BudgetChars: -1})
	if len(d.Incidents) != 1 || d.Incidents[0].Culprit != nil {
		t.Fatalf("incidents = %+v", d.Incidents)
	}
	raw := encode(t, d)
	if !strings.Contains(string(raw), `"culprit": null`) {
		t.Fatalf("a tie must say null explicitly:\n%s", raw)
	}
}
