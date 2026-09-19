// Package result defines result.json - the system of record - and builds it.
//
// Every other output is a pure function of this document: the terminal tables, the
// HTML report, the Markdown summary, the JUnit file, the agent digest and compare.
// A consequence worth stating plainly: a number that is not here cannot appear
// anywhere, which makes this file the design review for every feature.
//
// The shapes below mirror schemas/result.schema.json exactly, and a test validates
// every emitted document against that schema. See
// docs/adr/004-result-system-of-record.md for the versioning rules.
package result

// SchemaVersion is the version of the document shape this build writes.
//
// MAJOR.MINOR. Additive changes are MINOR and readers must ignore fields they do not
// recognise. Anything else is MAJOR and needs a migration.
const SchemaVersion = "1.0"

// Result is one run.
type Result struct {
	SchemaVersion string     `json:"schema_version"`
	Tool          Tool       `json:"tool"`
	Run           Run        `json:"run"`
	Config        *Config    `json:"config,omitempty"`
	Runners       []Runner   `json:"runners"`
	Telemetry     *Telemetry `json:"telemetry,omitempty"`
	Analysis      Analysis   `json:"analysis"`
	Capacity      *Capacity  `json:"capacity,omitempty"`
	Warnings      []Finding  `json:"warnings,omitempty"`
	Artifacts     *Artifacts `json:"artifacts,omitempty"`
}

// Tool is the binary that produced the document.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
	Go      string `json:"go,omitempty"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
}

// Run is the identity and timing of the run. Every offset in the document is
// milliseconds from one monotonic start shared by every runner and sampler.
type Run struct {
	ID                string            `json:"id"`
	Name              string            `json:"name,omitempty"`
	StartedAt         string            `json:"started_at"`
	FinishedAt        string            `json:"finished_at,omitempty"`
	Status            string            `json:"status"`
	DurationMS        float64           `json:"duration_ms"`
	ElapsedMS         float64           `json:"elapsed_ms,omitempty"`
	BucketMS          float64           `json:"bucket_ms"`
	BucketCount       int               `json:"bucket_count,omitempty"`
	WarmupMS          float64           `json:"warmup_ms,omitempty"`
	MinSamples        int               `json:"min_samples,omitempty"`
	Seed              uint64            `json:"seed"`
	Arrival           string            `json:"arrival,omitempty"`
	InterruptedReason string            `json:"interrupted_reason,omitempty"`
	Tags              map[string]string `json:"tags,omitempty"`
	Host              *Host             `json:"host,omitempty"`
}

// Run statuses.
const (
	StatusCompleted   = "completed"
	StatusInterrupted = "interrupted"
	StatusAborted     = "aborted"
	StatusFailed      = "failed"
)

// Host describes the generator machine, for judging whether it could have been the
// bottleneck.
type Host struct {
	Hostname      string `json:"hostname,omitempty"`
	CPUs          int    `json:"cpus,omitempty"`
	GOMAXPROCS    int    `json:"gomaxprocs,omitempty"`
	OpenFileLimit int    `json:"open_file_limit,omitempty"`
}

// Config is the configuration that actually ran, with every secret redacted. Keeping
// it here makes the result self-sufficient: compare can diff two runs' settings and
// `run --from` can reproduce one, without the original file.
type Config struct {
	Hash       string   `json:"hash"`
	Effective  any      `json:"effective,omitempty"`
	SourcePath string   `json:"source_path,omitempty"`
	Overrides  []string `json:"overrides,omitempty"`
	Policy     any      `json:"policy,omitempty"`
}

// Quantiles is a latency distribution in milliseconds.
type Quantiles struct {
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p999"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// Sketch is a serialised whole-run sketch, kept so compare can compute confidence
// intervals without the raw observations.
type Sketch struct {
	Encoding         string  `json:"encoding"`
	RelativeAccuracy float64 `json:"relative_accuracy"`
	Count            float64 `json:"count"`
	Data             string  `json:"data"`
}

// OpSummary is aggregate statistics over the measured window - the run excluding
// warm-up - for one runner or one label.
type OpSummary struct {
	N           int64            `json:"n"`
	OK          int64            `json:"ok"`
	ErrorsTotal int64            `json:"errors_total"`
	ErrorRatio  float64          `json:"error_ratio"`
	Errors      map[string]int64 `json:"errors,omitempty"`
	Response    Quantiles        `json:"response_ms"`
	Service     Quantiles        `json:"service_ms"`
	ClientWait  Quantiles        `json:"client_wait_ms"`
	AchievedRPS float64          `json:"achieved_rps"`
	BytesIn     int64            `json:"bytes_in"`
	BytesOut    int64            `json:"bytes_out"`
}

// Bucket is one sealed slice of the timeline.
type Bucket struct {
	Index        int              `json:"i"`
	OffsetMS     float64          `json:"t_ms"`
	N            int64            `json:"n"`
	Insufficient bool             `json:"insufficient,omitempty"`
	Warmup       bool             `json:"warmup,omitempty"`
	Response     Quantiles        `json:"response_ms"`
	Service      Quantiles        `json:"service_ms"`
	ClientWait   Quantiles        `json:"client_wait_ms"`
	Errors       map[string]int64 `json:"errors,omitempty"`
	ErrorsTotal  int64            `json:"errors_total"`
	RPS          float64          `json:"rps"`
	Offered      int64            `json:"offered"`
	Dropped      int64            `json:"dropped"`
	InFlightMax  int64            `json:"in_flight_max"`
	BytesIn      int64            `json:"bytes_in"`
}

// Target is a resolved destination, recorded for the same-host caveat and the safety
// trail. Credentials never appear here.
type Target struct {
	Host  string   `json:"host,omitempty"`
	Addrs []string `json:"addrs,omitempty"`
	Scope string   `json:"scope,omitempty"`
}

// Address scopes, which drive both the policy decision and the same-host caveat.
const (
	ScopeLoopback  = "loopback"
	ScopePrivate   = "private"
	ScopeLinkLocal = "link-local"
	ScopePublic    = "public"
)

// ExecutorStats is what the executor did, as opposed to what it was asked for.
type ExecutorStats struct {
	Type          string    `json:"type,omitempty"`
	Offered       int64     `json:"offered"`
	Dispatched    int64     `json:"dispatched"`
	Dropped       int64     `json:"dropped"`
	DroppedRatio  float64   `json:"dropped_ratio"`
	MaxInFlight   int       `json:"max_in_flight,omitempty"`
	PeakInFlight  int64     `json:"peak_in_flight,omitempty"`
	DispatchLagMS Quantiles `json:"dispatch_lag_ms"`
	Stages        []Stage   `json:"stages,omitempty"`
}

// Stage is one segment of the load profile that ran.
type Stage struct {
	DurationMS float64 `json:"duration_ms"`
	Target     float64 `json:"target"`
}

// HTTPDetail is the HTTP-specific part of a runner.
type HTTPDetail struct {
	StatusHistogram map[string]int64     `json:"status_histogram,omitempty"`
	PhasesMS        map[string]Quantiles `json:"phases_ms,omitempty"`
	ConnReuseRatio  float64              `json:"conn_reuse_ratio"`
	InsecureTLS     bool                 `json:"insecure_tls,omitempty"`
}

// Runner is everything recorded for one tier.
type Runner struct {
	Name           string               `json:"name"`
	Kind           string               `json:"kind"`
	Driver         string               `json:"driver,omitempty"`
	Targets        []Target             `json:"targets,omitempty"`
	Labels         []string             `json:"labels,omitempty"`
	Executor       *ExecutorStats       `json:"executor,omitempty"`
	Summary        OpSummary            `json:"summary"`
	LabelSummaries map[string]OpSummary `json:"labels_summary,omitempty"`
	Sketches       map[string]Sketch    `json:"sketches,omitempty"`
	Buckets        []Bucket             `json:"buckets"`
	LabelBuckets   map[string][]Bucket  `json:"label_buckets,omitempty"`
	HTTP           *HTTPDetail          `json:"http,omitempty"`
}

// Telemetry is server-side and generator-side health on the shared clock. Populated
// from phase 3.
type Telemetry struct {
	Generator *GeneratorSeries `json:"generator,omitempty"`
	Postgres  *SamplerSeries   `json:"postgres,omitempty"`
	MySQL     *SamplerSeries   `json:"mysql,omitempty"`
	Redis     *SamplerSeries   `json:"redis,omitempty"`
}

// GeneratorSeries is the load generator's own health, which is what distinguishes
// "the target is slow" from "we could not ask fast enough".
type GeneratorSeries struct {
	Samples []map[string]any `json:"samples,omitempty"`
}

// SamplerSeries is one datastore sampler's output.
type SamplerSeries struct {
	Available  bool             `json:"available"`
	Reason     string           `json:"reason,omitempty"`
	IntervalMS float64          `json:"interval_ms,omitempty"`
	Samples    []map[string]any `json:"samples,omitempty"`
	Statements []map[string]any `json:"statements,omitempty"`
}

// Finding is a coded observation. Findings never carry free-form target output.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Detail   any    `json:"detail,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// Severities. error invalidates a run, warn degrades it, info is context.
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Validity says whether the run can be believed at all. Read it before anything else.
type Validity struct {
	State    string    `json:"state"`
	Findings []Finding `json:"findings,omitempty"`
}

// Validity states.
const (
	ValidityValid    = "valid"
	ValidityDegraded = "degraded"
	ValidityInvalid  = "invalid"
)

// SLOCheck is one budget evaluation.
type SLOCheck struct {
	Runner string  `json:"runner"`
	Metric string  `json:"metric"`
	Budget float64 `json:"budget"`
	Actual float64 `json:"actual"`
	Pass   bool    `json:"pass"`
}

// SLO is the budget outcome for the run.
type SLO struct {
	Pass   bool       `json:"pass"`
	Checks []SLOCheck `json:"checks,omitempty"`
}

// Threshold records what a runner's hot-bucket threshold was and where it came from.
type Threshold struct {
	ThresholdMS float64 `json:"threshold_ms"`
	Source      string  `json:"source"`
}

// Evidence is one checkable observation behind a verdict.
type Evidence struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Data any    `json:"data,omitempty"`
}

// Verdict is the answer to the question the tool exists for.
type Verdict struct {
	Bottleneck string     `json:"bottleneck"`
	Confidence string     `json:"confidence"`
	Summary    string     `json:"summary"`
	Evidence   []Evidence `json:"evidence,omitempty"`
	Caveat     string     `json:"caveat,omitempty"`
	NextSteps  []string   `json:"next_steps,omitempty"`
}

// Bottleneck values.
const (
	BottleneckApp          = "app"
	BottleneckDB           = "db"
	BottleneckRedis        = "redis"
	BottleneckClient       = "client"
	BottleneckNone         = "none"
	BottleneckInconclusive = "inconclusive"
)

// Confidence levels.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// StandingCaveat is attached to every verdict. The tool observes association on a
// shared clock; it has no causal evidence, and says so every time it speaks.
const StandingCaveat = "Shared-clock correlation shows association, not causation."

// Analysis is everything derived from the recorded data, by pure functions of it.
type Analysis struct {
	Validity    Validity               `json:"validity"`
	SLO         SLO                    `json:"slo"`
	Thresholds  map[string]Threshold   `json:"thresholds,omitempty"`
	HotBuckets  map[string][]HotBucket `json:"hot_buckets,omitempty"`
	Incidents   []Incident             `json:"incidents,omitempty"`
	Correlation []Correlation          `json:"correlation,omitempty"`
	Verdict     Verdict                `json:"verdict"`
	Strain      *Strain                `json:"strain,omitempty"`
}

// HotBucket is a bucket judged abnormal, with the rule that fired.
type HotBucket struct {
	Index      int     `json:"i"`
	Reason     string  `json:"reason"`
	P99MS      float64 `json:"p99_ms,omitempty"`
	BaselineMS float64 `json:"baseline_ms,omitempty"`
	MZScore    float64 `json:"mz_score,omitempty"`
}

// Incident is a contiguous episode of abnormal latency, classified by which tiers were
// hot and which were demonstrably healthy.
type Incident struct {
	ID         string            `json:"id"`
	Class      string            `json:"class"`
	StartIndex int               `json:"start_i"`
	EndIndex   int               `json:"end_i"`
	StartS     float64           `json:"start_s,omitempty"`
	EndS       float64           `json:"end_s,omitempty"`
	DurationS  float64           `json:"duration_s,omitempty"`
	Runners    []IncidentRunner  `json:"runners,omitempty"`
	Culprit    *Culprit          `json:"culprit"`
	AppImpact  *AppImpact        `json:"app_impact,omitempty"`
	Telemetry  []TelemetrySignal `json:"telemetry,omitempty"`
	Confidence string            `json:"confidence,omitempty"`
}

// Incident classes.
const (
	IncidentCorrelated    = "correlated"
	IncidentStorageOnly   = "storage_only"
	IncidentAppOnly       = "app_only"
	IncidentClientLimited = "client_limited"
	IncidentUnobserved    = "unobserved"
)

// IncidentRunner is one runner's behaviour during an incident. A runner that stayed
// healthy is as much a part of the finding as one that did not.
type IncidentRunner struct {
	Name         string  `json:"name"`
	Hot          bool    `json:"hot"`
	FirstHotIdx  int     `json:"first_hot_i,omitempty"`
	PeakP99MS    float64 `json:"peak_p99_ms,omitempty"`
	BaselineP99  float64 `json:"baseline_p99_ms,omitempty"`
	Severity     float64 `json:"severity,omitempty"`
	Insufficient bool    `json:"insufficient,omitempty"`
}

// Culprit is the best-ranked tier inside a correlated incident. Ties are reported
// rather than broken arbitrarily.
type Culprit struct {
	Runner      string   `json:"runner"`
	Score       float64  `json:"score"`
	LeadBuckets int      `json:"lead_buckets,omitempty"`
	TiedWith    []string `json:"tied_with,omitempty"`
	Reasons     []string `json:"reasons,omitempty"`
}

// AppImpact is what users would have felt during an incident.
type AppImpact struct {
	PeakP99MS   float64 `json:"peak_p99_ms,omitempty"`
	SLOBreached bool    `json:"slo_breached,omitempty"`
	AffectedOps int64   `json:"affected_ops,omitempty"`
}

// TelemetrySignal is a server-side metric that moved in the same window, which is what
// raises a correlation to corroborated evidence.
type TelemetrySignal struct {
	Source   string  `json:"source"`
	Signal   string  `json:"signal"`
	Value    float64 `json:"value,omitempty"`
	Baseline float64 `json:"baseline,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// Correlation is how closely a storage probe tracked the application over the run.
type Correlation struct {
	Storage  string             `json:"storage"`
	Rho      float64            `json:"rho"`
	BestLag  int                `json:"best_lag_buckets"`
	RhoByLag map[string]float64 `json:"rho_by_lag,omitempty"`
	NBuckets int                `json:"n_buckets,omitempty"`
	Lean     string             `json:"lean,omitempty"`
}

// Strain is where latency started to degrade on a ramping run.
type Strain struct {
	Found         bool    `json:"found"`
	AtOffsetS     float64 `json:"at_offset_s,omitempty"`
	Users         float64 `json:"users,omitempty"`
	RPS           float64 `json:"rps,omitempty"`
	BaselineP99MS float64 `json:"baseline_p99_ms,omitempty"`
	Message       string  `json:"message,omitempty"`
	NextWindow    any     `json:"next_window,omitempty"`
}

// Capacity is the outcome of an auto-ramp search. Populated from phase 7.
type Capacity struct {
	Knob     string          `json:"knob"`
	Runner   string          `json:"runner,omitempty"`
	Levels   []CapacityLevel `json:"levels"`
	Boundary *Boundary       `json:"boundary,omitempty"`
	Knee     *float64        `json:"knee,omitempty"`
	USL      *USL            `json:"usl,omitempty"`
}

// CapacityLevel is one level tried during a capacity search.
type CapacityLevel struct {
	Level           int     `json:"level"`
	Phase           string  `json:"phase,omitempty"`
	Value           float64 `json:"value"`
	OK              bool    `json:"ok"`
	BreakReason     string  `json:"break_reason,omitempty"`
	OfferedRPS      float64 `json:"offered_rps,omitempty"`
	AchievedRPS     float64 `json:"achieved_rps,omitempty"`
	MeanConcurrency float64 `json:"mean_concurrency,omitempty"`
	P50MS           float64 `json:"p50_ms,omitempty"`
	P95MS           float64 `json:"p95_ms,omitempty"`
	P99MS           float64 `json:"p99_ms,omitempty"`
	ErrorRatio      float64 `json:"error_ratio,omitempty"`
	Culprit         *string `json:"culprit,omitempty"`
}

// Boundary is where capacity ran out, and whether re-running reproduced it.
type Boundary struct {
	LastOK      float64   `json:"last_ok,omitempty"`
	FirstBroken float64   `json:"first_broken,omitempty"`
	Stable      bool      `json:"stable"`
	Range       []float64 `json:"range,omitempty"`
}

// USL is a Universal Scalability Law fit, reported only when it is reliable enough to
// be worth quoting.
type USL struct {
	Fitted         bool    `json:"fitted"`
	Reason         string  `json:"reason,omitempty"`
	Lambda         float64 `json:"lambda,omitempty"`
	Sigma          float64 `json:"sigma,omitempty"`
	Kappa          float64 `json:"kappa,omitempty"`
	R2             float64 `json:"r2,omitempty"`
	PeakN          float64 `json:"peak_n,omitempty"`
	PeakThroughput float64 `json:"peak_throughput,omitempty"`
}

// Artifacts says where the run's files live.
type Artifacts struct {
	RunDir string `json:"run_dir,omitempty"`
	Result string `json:"result,omitempty"`
	Digest string `json:"digest,omitempty"`
	Report string `json:"report,omitempty"`
	Events string `json:"events,omitempty"`
	Config string `json:"config,omitempty"`
	Log    string `json:"log,omitempty"`
	Raw    string `json:"raw,omitempty"`
}

// RunnerByName finds a runner's record.
func (r *Result) RunnerByName(name string) *Runner {
	for i := range r.Runners {
		if r.Runners[i].Name == name {
			return &r.Runners[i]
		}
	}
	return nil
}
