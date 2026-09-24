package plan_test

import (
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/plan"
	"github.com/IshaanNene/Tracepoint/internal/policy"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// The offered load is the integral of the rate profile: a ramp from nothing to 100/s
// over 10s offers 500 operations, and a 30s hold at 100/s offers 3000.
func TestOfferedLoadIntegratesTheProfile(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.FromBytes("plan.yaml", []byte(`
version: 1
run: { duration: 40s, bucket: 1s, seed: 1 }
slo: { http: { p99: 250ms, error_rate: 0.01 } }
http:
  base_url: http://127.0.0.1:8080
  executor:
    stages: [{ duration: 10s, target: 100 }, { duration: 30s, target: 100 }]
  requests: [{ name: items, url: /api/items }]
`)), config.Options{Lookup: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Build(cfg, policy.ServerDefault(), []config.Warning{{Code: "W", Message: "a warning", Fix: "a fix"}}, "plan.yaml", []string{"run.seed=1"})

	if len(p.Runners) != 1 {
		t.Fatalf("runners = %+v", p.Runners)
	}
	r := p.Runners[0]
	if r.PeakRPS != 100 || r.Offered != 3500 || p.TotalOffered != 3500 {
		t.Fatalf("peak %g offered %g total %g, want 100, 3500, 3500", r.PeakRPS, r.Offered, p.TotalOffered)
	}
	if r.Profile != "0 to 100 over 10s, then hold 100 for 30s" {
		t.Fatalf("profile = %q", r.Profile)
	}
	if len(p.SLO) != 2 || p.SLO[0].Metric != "p99" || p.SLO[1].Budget != "1.00%" {
		t.Fatalf("slo = %+v", p.SLO)
	}
	if p.DurationS != 40 || p.Safety.AllowWrites || len(p.Warnings) != 1 {
		t.Fatalf("plan = %+v", p)
	}

	out := plan.Render(p)
	for _, want := range []string{"Plan for plan.yaml", "peak 100/s, about 3500 operations", "slo: http p99 <= 250ms", "! a warning", "nothing was contacted"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
