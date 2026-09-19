package result

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/executor"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
)

// RunnerInput is one runner's contribution to the result.
type RunnerInput struct {
	Snapshot metrics.Snapshot
	Stats    executor.Stats
	Driver   string
	Targets  []Target
	HTTP     *HTTPDetail
	Stages   []Stage
}

// BuildInput is everything needed to assemble a result.
type BuildInput struct {
	RunID       string
	Config      *config.Config
	Effective   any
	SourcePath  string
	Overrides   []string
	Seed        uint64
	StartedAt   time.Time
	FinishedAt  time.Time
	Elapsed     time.Duration
	Status      string
	Interrupted string
	Runners     []RunnerInput
	Warnings    []Finding
	Artifacts   *Artifacts
}

// Build assembles the result document.
//
// Nothing here talks to a target or a clock: it is a pure function of what was
// recorded, which is what makes the same run always produce the same document and
// makes every renderer reproducible from the file alone.
func Build(in BuildInput) (*Result, error) {
	bi := buildinfo.Get()
	r := &Result{
		SchemaVersion: SchemaVersion,
		Tool: Tool{
			Name: bi.Name, Version: bi.Version, Commit: bi.Commit,
			Date: bi.Date, Go: bi.Go, OS: bi.OS, Arch: bi.Arch,
		},
		Run: Run{
			ID:                in.RunID,
			StartedAt:         in.StartedAt.UTC().Format(time.RFC3339),
			Status:            in.Status,
			Seed:              in.Seed,
			InterruptedReason: in.Interrupted,
		},
		Warnings:  in.Warnings,
		Artifacts: in.Artifacts,
	}
	if !in.FinishedAt.IsZero() {
		r.Run.FinishedAt = in.FinishedAt.UTC().Format(time.RFC3339)
	}
	if in.Elapsed > 0 {
		r.Run.ElapsedMS = ms(in.Elapsed)
	}
	if c := in.Config; c != nil {
		r.Run.Name = c.Run.Name
		r.Run.DurationMS = ms(c.Run.Duration.D())
		r.Run.BucketMS = ms(c.Run.Bucket.D())
		r.Run.WarmupMS = ms(c.Run.Warmup.D())
		r.Run.MinSamples = c.Run.MinSamples
		r.Run.Arrival = c.Run.Arrival
		r.Run.Tags = c.Run.Tags
	}
	r.Run.Host = hostInfo()

	for _, ri := range in.Runners {
		r.Runners = append(r.Runners, buildRunner(ri))
	}
	if len(r.Runners) > 0 {
		r.Run.BucketCount = len(r.Runners[0].Buckets)
	}

	cfgDoc, err := buildConfig(in)
	if err != nil {
		return nil, err
	}
	r.Config = cfgDoc

	r.Analysis = Analyse(r, in.Config)
	return r, nil
}

func buildConfig(in BuildInput) (*Config, error) {
	if in.Config == nil {
		return nil, nil
	}
	effective := in.Effective
	if effective == nil {
		effective = in.Config
	}
	canonical, err := json.Marshal(effective)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "encoding the effective configuration")
	}
	sum := sha256.Sum256(canonical)

	// Round-trip through JSON so the embedded copy is plain data rather than Go types,
	// which keeps the document readable and schema-checkable.
	var plain any
	if err := json.Unmarshal(canonical, &plain); err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "normalising the effective configuration")
	}
	return &Config{
		Hash:       "sha256:" + hex.EncodeToString(sum[:]),
		Effective:  plain,
		SourcePath: in.SourcePath,
		Overrides:  in.Overrides,
	}, nil
}

func buildRunner(in RunnerInput) Runner {
	s := in.Snapshot
	out := Runner{
		Name:           s.Runner,
		Kind:           string(s.Kind),
		Driver:         in.Driver,
		Targets:        in.Targets,
		Labels:         s.Labels,
		Summary:        convertSummary(s.Summary),
		LabelSummaries: map[string]OpSummary{},
		Sketches:       map[string]Sketch{},
		Buckets:        convertBuckets(s.Buckets),
		HTTP:           in.HTTP,
	}
	for name, sum := range s.LabelSummaries {
		out.LabelSummaries[name] = convertSummary(sum)
	}
	for name, sk := range s.Sketches {
		out.Sketches[name] = Sketch{
			Encoding: sk.Encoding, RelativeAccuracy: sk.RelativeAccuracy,
			Count: sk.Count, Data: sk.Data,
		}
	}
	if len(s.LabelBuckets) > 0 {
		out.LabelBuckets = map[string][]Bucket{}
		for name, bs := range s.LabelBuckets {
			out.LabelBuckets[name] = convertBuckets(bs)
		}
	}
	if len(out.LabelSummaries) == 0 {
		out.LabelSummaries = nil
	}

	st := in.Stats
	out.Executor = &ExecutorStats{
		Type: "arrival-rate", Offered: st.Offered, Dispatched: st.Dispatched,
		Dropped: st.Dropped, MaxInFlight: st.MaxInFlight, PeakInFlight: st.PeakInFlight,
		DispatchLagMS: convertQuantiles(st.DispatchLag), Stages: in.Stages,
	}
	if st.Offered > 0 {
		out.Executor.DroppedRatio = float64(st.Dropped) / float64(st.Offered)
	}
	return out
}

func convertSummary(s metrics.OpSummary) OpSummary {
	return OpSummary{
		N: s.N, OK: s.OK, ErrorsTotal: s.ErrorsTotal, ErrorRatio: s.ErrorRatio,
		Errors: s.Errors, Response: convertQuantiles(s.Response),
		Service: convertQuantiles(s.Service), ClientWait: convertQuantiles(s.ClientWait),
		AchievedRPS: s.AchievedRPS, BytesIn: s.BytesIn, BytesOut: s.BytesOut,
	}
}

func convertBuckets(in []metrics.BucketSummary) []Bucket {
	out := make([]Bucket, 0, len(in))
	for _, b := range in {
		out = append(out, Bucket{
			Index: b.Index, OffsetMS: b.OffsetMS, N: b.N,
			Insufficient: b.Insufficient, Warmup: b.Warmup,
			Response: convertQuantiles(b.Response), Service: convertQuantiles(b.Service),
			ClientWait: convertQuantiles(b.ClientWait),
			Errors:     b.Errors, ErrorsTotal: b.ErrorsTotal, RPS: b.RPS,
			Offered: b.Offered, Dropped: b.Dropped, InFlightMax: b.InFlightMax, BytesIn: b.BytesIn,
		})
	}
	return out
}

func convertQuantiles(q metrics.Quantiles) Quantiles {
	return Quantiles{P50: round(q.P50), P90: round(q.P90), P95: round(q.P95),
		P99: round(q.P99), P999: round(q.P999), Max: round(q.Max), Mean: round(q.Mean)}
}

// round trims float noise so a renderer produces byte-identical output for the same
// run. Microsecond resolution is well below anything reported.
func round(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func hostInfo() *Host {
	h := &Host{CPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0)}
	if name, err := os.Hostname(); err == nil {
		h.Hostname = name
	}
	if n, ok := openFileLimit(); ok {
		h.OpenFileLimit = n
	}
	return h
}

// Encode writes the result as indented JSON with a trailing newline.
//
// Map keys are sorted by encoding/json, floats are already rounded, and nothing is
// stamped at encode time, so the same result encodes to the same bytes every time -
// which is what makes golden-file tests meaningful and lets a report be diffed.
func (r *Result) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "encoding the result")
	}
	return append(b, '\n'), nil
}

// Decode reads a result document, refusing one this build cannot interpret.
func Decode(data []byte) (*Result, error) {
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, errs.Wrap(errs.CodeResultParse, err, "this is not a TracePoint result document")
	}
	if probe.SchemaVersion == "" {
		return nil, errs.New(errs.CodeResultParse, "the document has no schema_version").
			WithHint("this is not a TracePoint result")
	}
	if major(probe.SchemaVersion) != major(SchemaVersion) {
		return nil, errs.New(errs.CodeResultSchemaUnsupported,
			"result schema %s cannot be read by this build, which writes %s",
			probe.SchemaVersion, SchemaVersion).
			WithHint("use a TracePoint whose major result version matches, or re-run the test")
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errs.Wrap(errs.CodeResultParse, err, "decoding the result")
	}
	return &r, nil
}

func major(v string) string {
	for i := range len(v) {
		if v[i] == '.' {
			return v[:i]
		}
	}
	return v
}

// SortedLabels returns a runner's label names in a stable order, so renderers produce
// the same output every time.
func (r *Runner) SortedLabels() []string {
	out := make([]string, 0, len(r.LabelSummaries))
	for k := range r.LabelSummaries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = fmt.Sprintf
