package ops

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Sections get_run_section can return.
var sections = []any{"incidents", "labels", "timeline", "telemetry", "levels"}

// SectionIn asks for one page of one section.
type SectionIn struct {
	RunID    string  `json:"run_id" jsonschema:"The run."`
	Section  string  `json:"section" jsonschema:"incidents: every incident in full. labels: per-label summaries. timeline: per-bucket numbers for one runner. telemetry: samples from one sampler. levels: capacity-search levels."`
	Runner   string  `json:"runner,omitempty" jsonschema:"For timeline, the runner (default: the application runner). For telemetry, the sampler: generator, postgres, mysql or redis. For labels, restricts to one runner."`
	FromS    float64 `json:"from_s,omitempty" jsonschema:"For timeline and telemetry, the start of the window in seconds from the run start."`
	ToS      float64 `json:"to_s,omitempty" jsonschema:"For timeline and telemetry, the end of the window; 0 means the end of the run."`
	Page     int     `json:"page,omitempty" jsonschema:"Page number, from 1."`
	PageSize int     `json:"page_size,omitempty" jsonschema:"Items per page, 1 to 200 (default 50)."`
}

// SectionOut is one page.
type SectionOut struct {
	RunID   string `json:"run_id"`
	Section string `json:"section"`
	Page    int    `json:"page"`
	Pages   int    `json:"pages"`
	Total   int    `json:"total"`
	Items   []any  `json:"items"`
}

func (o *SectionOut) summary() string {
	return fmt.Sprintf("%s page %d of %d (%d items)", o.Section, o.Page, o.Pages, o.Total)
}

func getRunSection() Operation {
	return define(Operation{
		Name:  "get_run_section",
		Title: "Get one section of a run's result",
		Description: "Return one page of one section of a finished run's result: incidents, per-label summaries, the per-bucket timeline of a runner, telemetry samples, or capacity levels. " +
			"Use it only when the digest is not enough - when it was truncated, or to check the evidence behind a verdict in a specific time window. Pages keep each response small.",
		Annotations: Annotations{ReadOnly: true, Idempotent: true},
	}, func(_ context.Context, s *Service, in *SectionIn) (*SectionOut, error) {
		r, err := s.run(in.RunID)
		if err != nil {
			return nil, err
		}
		res, err := readResult(r)
		if err != nil {
			return nil, err
		}
		items, err := sectionItems(res, in)
		if err != nil {
			return nil, err
		}
		return paginate(r.ID, in, items), nil
	}, func(in, _ *jsonschema.Schema) {
		in.Properties["section"].Enum = sections
		in.Properties["page"].Minimum = ptr(0.0)
		in.Properties["page_size"].Minimum = ptr(0.0)
		in.Properties["page_size"].Maximum = ptr(200.0)
	})
}

func sectionItems(res *result.Result, in *SectionIn) ([]any, error) {
	window := func(ms float64) bool {
		s := ms / 1000
		return s >= in.FromS && (in.ToS <= 0 || s < in.ToS)
	}
	var items []any
	switch in.Section {
	case "incidents":
		for _, inc := range res.Analysis.Incidents {
			items = append(items, inc)
		}
	case "labels":
		for i := range res.Runners {
			rn := &res.Runners[i]
			if in.Runner != "" && rn.Name != in.Runner {
				continue
			}
			for _, name := range rn.SortedLabels() {
				items = append(items, map[string]any{"runner": rn.Name, "label": name, "summary": rn.LabelSummaries[name]})
			}
		}
	case "timeline":
		rn := timelineRunner(res, in.Runner)
		if rn == nil {
			return nil, errs.New(errs.CodeOpsInvalidInput, "no runner named %q in this run", in.Runner).
				WithHint("runners: %v", runnerNames(res))
		}
		for _, b := range rn.Buckets {
			if !window(b.OffsetMS) {
				continue
			}
			items = append(items, map[string]any{
				"runner": rn.Name, "i": b.Index, "t_s": b.OffsetMS / 1000, "n": b.N,
				"response_p50_ms": b.Response.P50, "response_p99_ms": b.Response.P99,
				"service_p99_ms": b.Service.P99, "client_wait_p99_ms": b.ClientWait.P99,
				"errors_total": b.ErrorsTotal, "rps": b.RPS, "dropped": b.Dropped,
				"insufficient": b.Insufficient, "warmup": b.Warmup,
			})
		}
	case "telemetry":
		samples, err := telemetrySamples(res, in.Runner)
		if err != nil {
			return nil, err
		}
		for _, smp := range samples {
			if t, ok := smp["t_ms"].(float64); ok && window(t) {
				items = append(items, smp)
			}
		}
	case "levels":
		if res.Capacity != nil {
			for _, l := range res.Capacity.Levels {
				items = append(items, l)
			}
		}
	default:
		return nil, errs.New(errs.CodeOpsInvalidInput, "no section named %q", in.Section).
			WithHint("sections: incidents, labels, timeline, telemetry, levels")
	}
	return items, nil
}

func timelineRunner(res *result.Result, name string) *result.Runner {
	if name != "" {
		return res.RunnerByName(name)
	}
	for i := range res.Runners {
		if res.Runners[i].Kind == "app" {
			return &res.Runners[i]
		}
	}
	if len(res.Runners) > 0 {
		return &res.Runners[0]
	}
	return nil
}

func runnerNames(res *result.Result) []string {
	out := make([]string, 0, len(res.Runners))
	for _, r := range res.Runners {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func telemetrySamples(res *result.Result, source string) ([]map[string]any, error) {
	t := res.Telemetry
	if t == nil {
		return nil, nil
	}
	switch source {
	case "", "generator":
		if t.Generator != nil {
			return t.Generator.Samples, nil
		}
	case "postgres":
		if t.Postgres != nil {
			return t.Postgres.Samples, nil
		}
	case "mysql":
		if t.MySQL != nil {
			return t.MySQL.Samples, nil
		}
	case "redis":
		if t.Redis != nil {
			return t.Redis.Samples, nil
		}
	default:
		return nil, errs.New(errs.CodeOpsInvalidInput, "no telemetry source named %q", source).
			WithHint("sources: generator, postgres, mysql, redis")
	}
	return nil, nil
}

func paginate(runID string, in *SectionIn, items []any) *SectionOut {
	size := in.PageSize
	if size <= 0 {
		size = 50
	}
	page := in.Page
	if page <= 0 {
		page = 1
	}
	pages := (len(items) + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	out := &SectionOut{RunID: runID, Section: in.Section, Page: page, Pages: pages, Total: len(items), Items: []any{}}
	start := (page - 1) * size
	if start < len(items) {
		out.Items = items[start:min(start+size, len(items))]
	}
	return out
}
