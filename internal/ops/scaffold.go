package ops

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Scaffold writes a commented starter configuration. It is the one implementation
// behind both `tracepoint init` and scaffold_config.
//
// Everything that identifies a system - a DSN, an address - is written as an
// environment reference, never a value, so the file can be committed and shared.
func Scaffold(in ScaffoldIn) (string, []string, error) {
	var notes []string
	if in.BaseURL == "" {
		return "", nil, errs.New(errs.CodeOpsInvalidInput, "base_url is required").
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
	if _, err := time.ParseDuration(duration); err != nil {
		return "", nil, errs.New(errs.CodeOpsInvalidInput, "duration %q is not a duration", duration).
			WithHint("use a Go duration such as 30s or 3m")
	}
	paths := in.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
		notes = append(notes, "no paths were given, so the starter loads /; replace it with the endpoints that matter")
	}
	for _, name := range []string{in.DBDSNEnv, in.RedisAddrEnv} {
		if name != "" && !envName.MatchString(name) {
			return "", nil, errs.New(errs.CodeOpsInvalidInput, "%q is not an environment variable name", name).
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
  warmup: 5s        # excluded from every summary and baseline
slo:
  http: { p99: 250ms, error_rate: 0.01 }   # replace with the budgets that were agreed
http:
  base_url: %q
  executor: { type: arrival-rate, rate: %s, max_in_flight: 64 }
  requests:
`, duration, in.BaseURL, trimFloat(rate))
	for i, p := range paths {
		fmt.Fprintf(&b, "    - { name: %s, method: GET, url: %q }\n", requestName(p, i), p)
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
    - { name: probe, type: read, sql: "SELECT 1" }   # replace with a query the application runs
`, in.DBDriver, env)
		notes = append(notes, "SELECT 1 only shows that the database answers; a query on a table the application uses shows whether that table is contended")
	default:
		return "", nil, errs.New(errs.CodeOpsInvalidInput, "db_driver %q is not postgres, mysql or sqlite", in.DBDriver)
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
	return b.String(), notes, nil
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
