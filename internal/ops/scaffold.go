package ops

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/detect"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Scaffold writes a commented starter configuration. It is the one implementation
// behind `tracepoint init`, `tracepoint quick` and scaffold_config.
//
// Everything that identifies a system - a DSN, an address - is written as an
// environment reference, never a value, so the file can be committed and shared.
// With DetectDir it infers what it can from the project; with OpenAPIPath it imports
// requests. What is given explicitly always wins over what is inferred.
func Scaffold(in ScaffoldIn) (*ScaffoldOut, error) {
	out := &ScaffoldOut{}
	var notes []string
	var requests []detect.Request

	if in.DetectDir != "" {
		d, err := detect.Detect(in.DetectDir)
		if err != nil {
			return nil, err
		}
		out.Inferences = d.Inferences
		notes = append(notes, d.Notes...)
		if in.BaseURL == "" {
			in.BaseURL = d.BaseURL
		}
		if in.DBDriver == "" {
			in.DBDriver = d.DBDriver
		}
		if in.DBDSNEnv == "" && in.DBDriver != "" {
			in.DBDSNEnv = d.DBDSNEnv
		}
		if in.RedisAddrEnv == "" {
			in.RedisAddrEnv = d.RedisAddrEnv
		}
		if d.Import != nil && in.OpenAPIPath == "" && len(in.Paths) == 0 {
			requests = d.Import.Requests
			notes = append(notes, d.Import.Notes...)
		}
	}
	if in.OpenAPIPath != "" {
		if in.BaseURL == "" {
			return nil, errs.New(errs.CodeOpsInvalidInput, "importing an OpenAPI document needs base_url").
				WithHint("name the test environment's URL; a document's servers often name production, so they are never used by default")
		}
		data, err := os.ReadFile(in.OpenAPIPath)
		if err != nil {
			return nil, errs.Wrap(errs.CodeConfigNotFound, err, "reading the OpenAPI document %s", in.OpenAPIPath)
		}
		imp, err := detect.ImportOpenAPI(data, detect.OpenAPIOptions{})
		if err != nil {
			return nil, err
		}
		requests = imp.Requests
		notes = append(notes, imp.Notes...)
		if len(requests) == 0 {
			notes = append(notes, "the OpenAPI document has no safe operations to import, so the starter loads /")
		}
	}

	if in.BaseURL == "" {
		return nil, errs.New(errs.CodeOpsInvalidInput, "base_url is required").
			WithHint("for example http://127.0.0.1:8080 or ${BASE_URL}")
	}
	rate := in.Rate
	if rate <= 0 {
		rate = 20
	}
	duration := in.Duration
	if duration == "" {
		duration = "30s"
	}
	d, err := time.ParseDuration(duration)
	if err != nil || d <= 0 {
		return nil, errs.New(errs.CodeOpsInvalidInput, "duration %q is not a positive duration", duration).
			WithHint("use a Go duration such as 30s or 3m")
	}
	// Five seconds of warm-up, or a tenth of a run shorter than thirty.
	warmup := 5 * time.Second
	if d < 30*time.Second {
		warmup = (d / 10).Truncate(100 * time.Millisecond)
	}
	if len(requests) == 0 {
		paths := in.Paths
		if len(paths) == 0 {
			paths = []string{"/"}
			notes = append(notes, "no paths were given, so the starter loads /; replace it with the endpoints that matter")
		}
		for i, p := range paths {
			requests = append(requests, detect.Request{Name: requestName(p, i), Method: "GET", URL: p})
		}
	}
	for _, name := range []string{in.DBDSNEnv, in.RedisAddrEnv} {
		if name != "" && !envName.MatchString(name) {
			return nil, errs.New(errs.CodeOpsInvalidInput, "%q is not an environment variable name", name).
				WithHint("pass the name, such as DATABASE_URL, not the value")
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, `# TracePoint starter configuration. Validate it with `+"`tracepoint validate`"+`, check the
# load with `+"`tracepoint run --dry-run`"+`, then start with a short smoke run.
version: 1
run:
  duration: %s     # how long load is offered
  bucket: 1s        # every tier is recorded into the same one-second buckets
  warmup: %s        # excluded from every summary and baseline
slo:
  http: { p99: 250ms, error_rate: 0.01 }   # replace with the budgets that were agreed
http:
  base_url: %q
  executor: { type: arrival-rate, rate: %s, max_in_flight: 64 }
  requests:
`, duration, warmup, in.BaseURL, trimFloat(rate))
	for _, r := range requests {
		if r.Todo != "" {
			fmt.Fprintf(&b, "    # TODO: %s\n", r.Todo)
		}
		fmt.Fprintf(&b, "    - { name: %s, method: %s, url: %q }\n", r.Name, r.Method, r.URL)
	}

	switch in.DBDriver {
	case "":
	case "postgres", "mysql", "sqlite":
		env := in.DBDSNEnv
		if env == "" {
			env = "DATABASE_URL"
			notes = append(notes, "the database DSN is read from ${DATABASE_URL}; set it, or pass db_dsn_env")
		}
		fmt.Fprintf(&b, `db:
  # The probe queries the database directly, so its latency is that tier's health while
  # the application is under load. Keep it read-only unless a human has allowed writes.
  driver: %s
  dsn: "${%s}"
  executor: { type: arrival-rate, rate: 30 }
  queries:
    - { name: probe, type: read, sql: %q }%s
`, in.DBDriver, env, probeSQL(in.DBQuery), probeComment(in.DBQuery))
		if in.DBQuery == "" {
			notes = append(notes, "SELECT 1 only shows that the database answers; a query on a table the application uses shows whether that table is contended")
		}
	default:
		return nil, errs.New(errs.CodeOpsInvalidInput, "db_driver %q is not postgres, mysql or sqlite", in.DBDriver)
	}

	if in.RedisAddrEnv != "" {
		fmt.Fprintf(&b, `redis:
  addr: "${%s}"
  executor: { type: arrival-rate, rate: 50 }
  commands:
    - { name: probe, type: read, cmd: [GET, "tracepoint:probe"] }   # a miss is an answer, not an error
`, in.RedisAddrEnv)
	}

	var samplers []string
	switch in.DBDriver {
	case "postgres", "mysql":
		samplers = append(samplers, in.DBDriver+": true")
	}
	if in.RedisAddrEnv != "" {
		samplers = append(samplers, "redis: true")
	}
	if len(samplers) > 0 {
		fmt.Fprintf(&b, "telemetry: { %s }   # server-side signals that corroborate a verdict\n", strings.Join(samplers, ", "))
	}
	notes = append(notes, "ask a human which targets and environments may be loaded before running this; never point it at production without explicit allowlisting")
	out.ConfigYAML, out.Notes = b.String(), notes
	return out, nil
}

func requestName(path string, i int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(path) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "root"
	}
	if i > 0 {
		name = fmt.Sprintf("%s-%d", name, i+1)
	}
	return name
}

func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

func probeSQL(q string) string {
	if q == "" {
		return "SELECT 1"
	}
	return q
}

func probeComment(q string) string {
	if q == "" {
		return "   # replace with a query the application runs"
	}
	return ""
}
