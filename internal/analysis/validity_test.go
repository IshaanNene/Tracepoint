package analysis

import (
	"strings"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

func codes(v result.Validity) []string {
	out := make([]string, 0, len(v.Findings))
	for _, f := range v.Findings {
		out = append(out, f.Code)
	}
	return out
}

func hasCode(v result.Validity, code string) bool {
	for _, c := range codes(v) {
		if c == code {
			return true
		}
	}
	return false
}

// dropping marks buckets from..to as having dropped arrivals, with everything offered.
func dropping(r *result.Result, runner string, from, to int) {
	rn := r.RunnerByName(runner)
	for i := range rn.Buckets {
		rn.Buckets[i].Offered = 120
		if i >= from && i <= to {
			rn.Buckets[i].Dropped = 20
		}
	}
	rn.Executor = &result.ExecutorStats{Offered: 120 * int64(len(rn.Buckets)), Dropped: 20 * int64(to-from+1), MaxInFlight: 4}
	rn.Executor.DroppedRatio = float64(rn.Executor.Dropped) / float64(rn.Executor.Offered)
}

func TestValidityClientCapped(t *testing.T) {
	r := newScenario(60).result()
	dropping(r, "http", 20, 50)
	r.Runners[0].Summary.Service.P99 = 20
	v := evaluateValidity(r, Inputs{})
	if v.State != result.ValidityInvalid || !hasCode(v, CodeClientCapped) {
		t.Fatalf("got %s %v, want invalid CLIENT_CAPPED", v.State, codes(v))
	}
	f := v.Findings[0]
	// 120 offered per 1s bucket, 20ms p99, 20% headroom: ceil(2.88) = 3, which is
	// below the 4 configured, so the advice is to double.
	if !strings.Contains(f.Fix, "max_in_flight to about 8") {
		t.Fatalf("fix = %q", f.Fix)
	}
}

func TestValiditySaturated(t *testing.T) {
	r := newScenario(60).set("http", 20, 50, 400).result()
	dropping(r, "http", 20, 50)
	v := evaluateValidity(r, Inputs{})
	if v.State != result.ValidityDegraded || !hasCode(v, CodeTargetSaturated) {
		t.Fatalf("got %s %v, want degraded TARGET_SATURATED", v.State, codes(v))
	}
}

// When every bucket dropped, the earliest ones are the only baseline.
func TestServiceTimeRoseAllDropping(t *testing.T) {
	vals := with(steady(10, 20), steady(20, 60)...)
	r := &result.Result{Runners: []result.Runner{{Name: "http", Buckets: series(vals...)}}}
	for i := range r.Runners[0].Buckets {
		r.Runners[0].Buckets[i].Dropped = 1
	}
	if !serviceTimeRose(&r.Runners[0]) {
		t.Fatalf("a tripling from the first buckets should count as rising")
	}
	flat := &result.Runner{Buckets: series(steady(30, 20)...)}
	for i := range flat.Buckets {
		flat.Buckets[i].Dropped = 1
	}
	if serviceTimeRose(flat) {
		t.Fatalf("a flat line is not rising")
	}
	if serviceTimeRose(&result.Runner{Buckets: series(steady(3, 20)...)}) {
		t.Fatalf("nothing dropped, nothing rose")
	}
}

func TestValidityGeneratorBehind(t *testing.T) {
	r := newScenario(10).result()
	r.Runners[0].Executor = &result.ExecutorStats{DispatchLagMS: result.Quantiles{P99: 49}}
	// 5% of a 1s bucket is 50ms, which is above the 5ms floor.
	if v := evaluateValidity(r, Inputs{}); v.State != result.ValidityValid {
		t.Fatalf("49ms of lag against a 50ms limit: %s %v", v.State, codes(v))
	}
	r.Runners[0].Executor.DispatchLagMS.P99 = 51
	if v := evaluateValidity(r, Inputs{}); !hasCode(v, CodeGeneratorBehind) {
		t.Fatalf("51ms: %v", codes(v))
	}
}

func TestValidityTelemetryUnavailable(t *testing.T) {
	r := newScenario(10).result()
	r.Telemetry = &result.Telemetry{
		Postgres: &result.SamplerSeries{Available: false, Reason: "permission denied for pg_stat_activity"},
		Redis:    &result.SamplerSeries{Available: true, Samples: []map[string]any{{"t_ms": 0.0}}},
	}
	v := evaluateValidity(r, Inputs{Telemetry: []string{"postgres", "redis", "mysql"}})
	if v.State != result.ValidityDegraded {
		t.Fatalf("state = %s", v.State)
	}
	var msgs []string
	for _, f := range v.Findings {
		if f.Code == CodeTelemetryUnavailable {
			msgs = append(msgs, f.Message)
		}
	}
	if len(msgs) != 2 || !strings.Contains(msgs[0], "permission denied") || !strings.Contains(msgs[1], "mysql") {
		t.Fatalf("findings = %v", msgs)
	}
}

func TestValidityLittlesLaw(t *testing.T) {
	r := newScenario(20).result()
	for i := range r.Runners[0].Buckets {
		b := &r.Runners[0].Buckets[i]
		b.RPS, b.Service.Mean, b.InFlightMax = 100, 200, 5 // implies 20 in flight
	}
	v := evaluateValidity(r, Inputs{})
	if !hasCode(v, CodeLittlesLaw) {
		t.Fatalf("codes = %v", codes(v))
	}
	for i := range r.Runners[0].Buckets {
		r.Runners[0].Buckets[i].InFlightMax = 25
	}
	if v := evaluateValidity(r, Inputs{}); hasCode(v, CodeLittlesLaw) {
		t.Fatalf("25 in flight covers 20: %v", codes(v))
	}
}

func TestValidityHTTPFindings(t *testing.T) {
	r := newScenario(10).result()
	r.Runners[0].Summary.N = 100
	r.Runners[0].HTTP = &result.HTTPDetail{InsecureTLS: true, StatusHistogram: map[string]int64{"429": 6}}
	r.Run.Status = result.StatusAborted
	v := evaluateValidity(r, Inputs{})
	for _, c := range []string{CodeInsecureTLS, CodeRateLimited, CodeRunAborted} {
		if !hasCode(v, c) {
			t.Errorf("missing %s in %v", c, codes(v))
		}
	}
}

func TestValiditySameHostDeduplicated(t *testing.T) {
	r := newScenario(10).result()
	for i := range r.Runners {
		r.Runners[i].Targets = []result.Target{{Host: "localhost", Scope: result.ScopeLoopback}}
	}
	v := evaluateValidity(r, Inputs{})
	if n := len(v.Findings); n != 1 {
		t.Fatalf("%d findings, want the caveat once: %v", n, codes(v))
	}
}
