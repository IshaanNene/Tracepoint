package analysis

import (
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
)

// Every analysis this package can produce must fit the result contract. The scenarios
// cover each incident class, a tie, a culprit, telemetry signals and each verdict.
func TestAnalysisSatisfiesResultSchema(t *testing.T) {
	scenarios := map[string]*result.Result{
		"correlated": newScenario(90).set("db", 30, 34, 900).set("http", 31, 35, 950).result(),
		"tie":        newScenario(90).set("db", 30, 33, 400).set("redis", 30, 33, 400).set("http", 30, 33, 900).result(),
		"masked":     newScenario(90).set("db", 30, 34, 900).result(),
		"app":        newScenario(90).set("http", 30, 34, 900).result(),
		"client":     newScenario(90).set("http", 30, 60, 300).waitAt("http", 30, 60, 4000).result(),
		"unobserved": newScenario(90).drop("db").drop("redis").set("http", 30, 34, 900).result(),
		"clean":      newScenario(90).result(),
	}
	withTelemetry := newScenario(90).set("db", 30, 34, 900).set("http", 31, 35, 950).result()
	withTelemetry.Telemetry = locksTelemetry(90, 30, 34)
	scenarios["telemetry"] = withTelemetry

	invalid := newScenario(30).result()
	invalid.Runners[0].Executor = &result.ExecutorStats{DispatchLagMS: result.Quantiles{P99: 400}}
	scenarios["invalid"] = invalid

	for name, r := range scenarios {
		t.Run(name, func(t *testing.T) {
			r.Analysis = Analyse(r, Inputs{})
			doc, err := r.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			schematest.Validate(t, schemas.Result, doc)
		})
	}
}
