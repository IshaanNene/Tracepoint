package httprun_test

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/runner/httprun"
)

func TestMain(m *testing.M) {
	// The default transport keeps idle connections alive past a test; that is the
	// transport's job, not a leak in ours.
	goleak.VerifyTestMain(m, goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"))
}

func ptr[T any](v T) *T { return &v }

type harness struct {
	runner    *httprun.Runner
	collector *metrics.Collector
	deps      runner.Deps
}

func newHarness(t *testing.T, cfg *config.HTTP, labels ...string) *harness {
	t.Helper()
	col, err := metrics.NewCollector(metrics.Config{
		Runner: "http", Kind: metrics.KindApp, Labels: labels,
		BucketWidth: time.Second, SealDelay: 5 * time.Second, MinSamples: 1, TrackStatuses: true,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	deps := runner.Deps{
		RunID: "20260920T120000Z-test01", Collector: col,
		Clock: clock.New(), Start: time.Now(), Seed: 7,
	}
	r, err := httprun.New(cfg, 10*time.Second, 5*time.Second, deps)
	if err != nil {
		t.Fatalf("httprun.New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &harness{runner: r, collector: col, deps: deps}
}

func (h *harness) do(t *testing.T, n int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range n {
		it := runner.Iteration{
			Index: int64(i), Intended: time.Duration(i) * time.Millisecond,
			Dispatched:  time.Duration(i) * time.Millisecond,
			WorkerStart: h.deps.Elapsed(), Rand: rng,
		}
		if err := h.runner.Do(context.Background(), &it, h.collector); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	h.collector.Finish(time.Hour)
}

func simpleConfig(url string, expect *config.Expect) *config.HTTP {
	return &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 10, MaxInFlight: 8},
		Requests: []config.HTTPStep{{Name: "probe", Method: "GET", URL: url, Expect: expect}},
	}
}

func TestSuccessfulRequestIsRecorded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "hello world")
	}))
	defer srv.Close()

	h := newHarness(t, simpleConfig(srv.URL, nil), "probe")
	h.do(t, 5)

	s := h.collector.Snapshot().Summary
	if s.N != 5 || s.OK != 5 {
		t.Errorf("n=%d ok=%d, want 5/5 (errors: %v)", s.N, s.OK, s.Errors)
	}
	if s.BytesIn != int64(5*len("hello world")) {
		t.Errorf("bytes in = %d, want %d; bodies must be read, not skipped", s.BytesIn, 5*len("hello world"))
	}
	// Service time is measured from the moment a connection was in hand, so it must be
	// at or below response time, which is measured from the scheduled send.
	if s.Service.P50 > s.Response.P50 {
		t.Errorf("service p50 %v exceeds response p50 %v", s.Service.P50, s.Response.P50)
	}
	if h.collector.Snapshot().StatusHistogram["200"] != 5 {
		t.Errorf("status histogram = %v, want five 200s", h.collector.Snapshot().StatusHistogram)
	}
}

// Test traffic must be identifiable in the target's own logs, so a team can filter it
// out of their dashboards - or into them.
func TestTrafficIsIdentifiable(t *testing.T) {
	t.Parallel()
	var gotUA, gotRunID atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA.Store(r.Header.Get("User-Agent"))
		gotRunID.Store(r.Header.Get(httprun.RunIDHeader))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newHarness(t, simpleConfig(srv.URL, nil), "probe")
	h.do(t, 1)

	ua, _ := gotUA.Load().(string)
	if !strings.HasPrefix(ua, "tracepoint/") || !strings.Contains(ua, "+run=20260920T120000Z-test01") {
		t.Errorf("User-Agent = %q, want it to name the tool and the run", ua)
	}
	if id, _ := gotRunID.Load().(string); id != "20260920T120000Z-test01" {
		t.Errorf("%s = %q, want the run id", httprun.RunIDHeader, id)
	}
}

func TestStatusClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		expect *config.Expect
		want   string
	}{
		{"200 with no expectation", 200, nil, "ok"},
		{"301 with no expectation", 301, nil, "ok"},
		{"404 with no expectation", 404, nil, "http_4xx"},
		{"500 with no expectation", 500, nil, "http_5xx"},
		{"429 with no expectation", 429, nil, "http_4xx"},
		{"201 when 201 is expected", 201, &config.Expect{Status: []int{201}}, "ok"},
		{"200 when only 201 is expected", 200, &config.Expect{Status: []int{201}}, "expect_failed"},
		{"503 when only 201 is expected", 503, &config.Expect{Status: []int{201}}, "http_5xx"},
		{"404 when only 201 is expected", 404, &config.Expect{Status: []int{201}}, "http_4xx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			h := newHarness(t, simpleConfig(srv.URL, tc.expect), "probe")
			h.do(t, 1)

			s := h.collector.Snapshot().Summary
			if tc.want == "ok" {
				if s.OK != 1 {
					t.Errorf("status %d: ok=%d, want 1 (errors %v)", tc.status, s.OK, s.Errors)
				}
				return
			}
			if s.Errors[tc.want] != 1 {
				t.Errorf("status %d: errors = %v, want one %s", tc.status, s.Errors, tc.want)
			}
		})
	}
}

// A 429 is the target working as designed, so it must not trip the abort guard - but
// it must still be visible, because a run full of them is measuring a rate limiter.
func TestRateLimitedResponsesAreVisibleButDoNotCountAsFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	h := newHarness(t, simpleConfig(srv.URL, nil), "probe")
	h.do(t, 3)
	snap := h.collector.Snapshot()

	if got := snap.StatusHistogram["429"]; got != 3 {
		t.Errorf("status histogram = %v, want three 429s", snap.StatusHistogram)
	}
	if got := snap.Summary.Errors["http_4xx"]; got != 3 {
		t.Errorf("errors = %v, want three http_4xx", snap.Summary.Errors)
	}
	if metrics.ClassHTTP4xx.CountsTowardAbortGuard() {
		t.Error("a 4xx must not trip the abort guard")
	}
}

func TestTimeoutIsClassifiedAsTimeoutNotAsAConnectionFailure(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() { close(release); srv.Close() }()

	cfg := simpleConfig(srv.URL, nil)
	cfg.Requests[0].Timeout = ptr(config.Duration(80 * time.Millisecond))
	h := newHarness(t, cfg, "probe")
	h.do(t, 1)

	s := h.collector.Snapshot().Summary
	if s.Errors["timeout"] != 1 {
		t.Errorf("errors = %v, want one timeout", s.Errors)
	}
	// The stall must be measured, not discarded: response time includes it.
	if s.Response.Max < 70 {
		t.Errorf("response max = %vms, want at least the 80ms the request waited", s.Response.Max)
	}
}

func TestConnectionRefusedIsClassified(t *testing.T) {
	t.Parallel()
	// A port nothing is listening on: reserve one, then release it.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()

	h := newHarness(t, simpleConfig(dead, nil), "probe")
	h.do(t, 1)

	s := h.collector.Snapshot().Summary
	if s.Errors["connection"] != 1 {
		t.Errorf("errors = %v, want one connection failure", s.Errors)
	}
	if s.N != 1 {
		t.Errorf("n = %d; a failed request is still an observation", s.N)
	}
}

func TestWeightedSelection(t *testing.T) {
	t.Parallel()
	var a, b atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/a") {
			a.Add(1)
		} else {
			b.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 10, MaxInFlight: 4},
		Requests: []config.HTTPStep{
			{Name: "heavy", Method: "GET", URL: srv.URL + "/a", Weight: ptr(9.0)},
			{Name: "light", Method: "GET", URL: srv.URL + "/b", Weight: ptr(1.0)},
		},
	}
	h := newHarness(t, cfg, "heavy", "light")
	h.do(t, 400)

	got, other := a.Load(), b.Load()
	ratio := float64(got) / float64(got+other)
	if ratio < 0.8 || ratio > 0.97 {
		t.Errorf("the 9:1 weighting produced %d:%d (%.2f); want roughly 0.9", got, other, ratio)
	}
	// Labels must be kept apart, not merged.
	snap := h.collector.Snapshot()
	if snap.LabelSummaries["heavy"].N+snap.LabelSummaries["light"].N != 400 {
		t.Errorf("per-label counts do not add up: %v", snap.LabelSummaries)
	}
}

func TestBaseURLResolvesRelativePaths(t *testing.T) {
	t.Parallel()
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.HTTP{
		BaseURL:  srv.URL,
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Requests: []config.HTTPStep{{Name: "probe", Method: "GET", URL: "/api/items"}},
	}
	h := newHarness(t, cfg, "probe")
	h.do(t, 1)

	if got, _ := path.Load().(string); got != "/api/items" {
		t.Errorf("path = %q, want /api/items", got)
	}
}

func TestRelativeURLWithoutBaseIsRejected(t *testing.T) {
	t.Parallel()
	cfg := &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Requests: []config.HTTPStep{{Name: "probe", Method: "GET", URL: "/api/items"}},
	}
	col, err := metrics.NewCollector(metrics.Config{
		Runner: "http", Labels: []string{"probe"}, BucketWidth: time.Second, MinSamples: 1,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	_, err = httprun.New(cfg, time.Second, time.Second, runner.Deps{
		Collector: col, Clock: clock.New(), Start: time.Now(),
	})
	if err == nil {
		t.Fatal("a relative url with no base_url should be rejected before the run starts")
	}
}

// A literal {{token}} must never reach the target: it would request a resource that
// does not exist and the 404s would look like a target problem (spec §4).
func TestUnexpandedTemplateIsRejected(t *testing.T) {
	t.Parallel()
	cfg := &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Requests: []config.HTTPStep{{Name: "probe", Method: "GET", URL: "http://127.0.0.1/items/{{randInt 1 10}}"}},
	}
	col, _ := metrics.NewCollector(metrics.Config{
		Runner: "http", Labels: []string{"probe"}, BucketWidth: time.Second, MinSamples: 1,
	})
	_, err := httprun.New(cfg, time.Second, time.Second, runner.Deps{
		Collector: col, Clock: clock.New(), Start: time.Now(),
	})
	if err == nil {
		t.Fatal("a template this build cannot expand must be rejected, not sent literally")
	}
	if !strings.Contains(err.Error(), "template") {
		t.Errorf("error should explain the template: %v", err)
	}
}

func TestPostBodyIsSent(t *testing.T) {
	t.Parallel()
	var body atomic.Value
	var contentType atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		contentType.Store(r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Requests: []config.HTTPStep{{
			Name: "create", Method: "POST", URL: srv.URL,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"title":"x"}`,
			Expect:  &config.Expect{Status: []int{201}},
		}},
	}
	h := newHarness(t, cfg, "create")
	h.do(t, 1)

	if got, _ := body.Load().(string); got != `{"title":"x"}` {
		t.Errorf("body = %q", got)
	}
	if got, _ := contentType.Load().(string); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if s := h.collector.Snapshot().Summary; s.OK != 1 {
		t.Errorf("ok = %d, want 1 (errors %v)", s.OK, s.Errors)
	}
	if s := h.collector.Snapshot().Summary; s.BytesOut != int64(len(`{"title":"x"}`)) {
		t.Errorf("bytes out = %d, want %d", s.BytesOut, len(`{"title":"x"}`))
	}
}

func TestGlobalHeadersAreMergedAndOverridable(t *testing.T) {
	t.Parallel()
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("X-Env") + "|" + r.Header.Get("X-Both"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.HTTP{
		Headers:  map[string]string{"X-Env": "staging", "X-Both": "global"},
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Requests: []config.HTTPStep{{
			Name: "probe", Method: "GET", URL: srv.URL,
			Headers: map[string]string{"X-Both": "per-request"},
		}},
	}
	h := newHarness(t, cfg, "probe")
	h.do(t, 1)

	if v, _ := got.Load().(string); v != "staging|per-request" {
		t.Errorf("headers = %q, want the global header applied and the per-request one winning", v)
	}
}

func TestPreflightCatchesADeadTargetBeforeAnyLoad(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()

	h := newHarness(t, simpleConfig(dead, nil), "probe")
	err := h.runner.Prepare(context.Background())
	if err == nil {
		t.Fatal("preflight should fail against a target that is not listening")
	}
	if !strings.Contains(err.Error(), "preflight") {
		t.Errorf("error should say it was preflight: %v", err)
	}
}

func TestPreflightPassesAgainstALiveTarget(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Requests: []config.HTTPStep{
			{Name: "a", Method: "GET", URL: srv.URL + "/a"},
			{Name: "b", Method: "GET", URL: srv.URL + "/b"},
		},
	}
	h := newHarness(t, cfg, "a", "b")
	if err := h.runner.Prepare(context.Background()); err != nil {
		t.Fatalf("preflight against a live target: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("preflight made %d requests, want one per label", got)
	}
}

// A target failing before the test has even started is worth refusing to run against.
func TestPreflightRefusesATargetAlreadyReturning5xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newHarness(t, simpleConfig(srv.URL, nil), "probe")
	if err := h.runner.Prepare(context.Background()); err == nil {
		t.Fatal("preflight should refuse a target that is already returning 500")
	}
}

func TestTargetsAreReported(t *testing.T) {
	t.Parallel()
	cfg := &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Requests: []config.HTTPStep{
			{Name: "a", Method: "GET", URL: "http://127.0.0.1:8080/a"},
			{Name: "b", Method: "GET", URL: "http://127.0.0.1:8080/b"},
			{Name: "c", Method: "GET", URL: "http://other.internal/c"},
		},
	}
	h := newHarness(t, cfg, "a", "b", "c")
	got := h.runner.Targets()
	if len(got) != 2 {
		t.Errorf("Targets() = %v, want the two distinct hosts", got)
	}
}

func TestRunnerMetadata(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleConfig("http://127.0.0.1:1/", nil), "probe")
	if h.runner.Name() != "http" {
		t.Errorf("Name() = %q", h.runner.Name())
	}
	if h.runner.Kind() != metrics.KindApp {
		t.Errorf("Kind() = %q, want app; http is the tier users talk to", h.runner.Kind())
	}
	if got := h.runner.Labels(); len(got) != 1 || got[0] != "probe" {
		t.Errorf("Labels() = %v", got)
	}
	if h.runner.InsecureTLS() {
		t.Error("InsecureTLS() true without it being configured")
	}
}

func TestJourneysAreRejectedForNow(t *testing.T) {
	t.Parallel()
	cfg := &config.HTTP{
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Journeys: []config.Journey{{Name: "j", Steps: []config.HTTPStep{{Name: "s", URL: "http://127.0.0.1/"}}}},
	}
	col, _ := metrics.NewCollector(metrics.Config{
		Runner: "http", Labels: []string{"j/s"}, BucketWidth: time.Second, MinSamples: 1,
	})
	_, err := httprun.New(cfg, time.Second, time.Second, runner.Deps{
		Collector: col, Clock: clock.New(), Start: time.Now(),
	})
	if err == nil {
		t.Fatal("journeys are not implemented yet and must be refused rather than silently ignored")
	}
}
