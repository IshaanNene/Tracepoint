package httprun_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/runner/httprun"
)

func dur(d time.Duration) *config.Duration { v := config.Duration(d); return &v }

// shop is a target with a login that returns a token and sets a session cookie, and
// a profile that demands both.
type shop struct {
	*httptest.Server
	profileHits atomic.Int64
	mu          sync.Mutex
	cookiesSeen []string
	traces      []string
}

func newShop(t *testing.T, loginBody string) *shop {
	t.Helper()
	s := &shop{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.traces = append(s.traces, r.Header.Get("traceparent"))
		if c, err := r.Cookie("session"); err == nil {
			s.cookiesSeen = append(s.cookiesSeen, r.URL.Path+"="+c.Value)
		} else {
			s.cookiesSeen = append(s.cookiesSeen, r.URL.Path+"=")
		}
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Path: "/"})
			w.Header().Set("X-Cart", "cart-9")
			_, _ = w.Write([]byte(loginBody))
		case strings.HasPrefix(r.URL.Path, "/users/"):
			s.profileHits.Add(1)
			c, err := r.Cookie("session")
			if r.Header.Get("Authorization") != "Bearer abc" || err != nil || c.Value != "s1" || r.URL.Path != "/users/7" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok","count":3}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func checkout(base string, profileExpect *config.Expect, think *config.ThinkTime) *config.HTTP {
	return &config.HTTP{
		BaseURL:  base,
		Executor: config.Executor{Type: "arrival-rate", Rate: 1, MaxInFlight: 1},
		Journeys: []config.Journey{{Name: "checkout", Steps: []config.HTTPStep{
			{Name: "login", Method: "POST", URL: "/login", Think: think, Extract: []config.Extract{
				{Name: "token", From: "body", Path: "token"}, {Name: "uid", From: "body", Path: "user.id"},
				{Name: "cart", From: "header", Path: "X-Cart"},
			}},
			{Name: "profile", Method: "GET", URL: "/users/{{uid}}", Headers: map[string]string{"Authorization": "Bearer {{token}}"}, Expect: profileExpect},
		}}},
	}
}

func label(h *harness, name string) metrics.OpSummary {
	return h.collector.Snapshot().LabelSummaries[name]
}

// Steps run in order, each extraction feeds the next step, and within one iteration
// the cookie the first step set reaches the second.
func TestJourneyExtractsAndCarriesCookies(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{"token":"abc","user":{"id":7}}`)
	h := newHarness(t, checkout(s.URL, &config.Expect{Status: []int{200}}, nil), "checkout/login", "checkout/profile")
	h.do(t, 3)
	for _, l := range []string{"checkout/login", "checkout/profile"} {
		if sum := label(h, l); sum.N != 3 || sum.OK != 3 {
			t.Fatalf("%s: %+v", l, sum)
		}
	}
}

// An extraction that finds nothing fails its step as extract_failed and ends the
// iteration: the next step is never sent.
func TestExtractionFailureEndsTheIteration(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{"user":{}}`)
	h := newHarness(t, checkout(s.URL, nil, nil), "checkout/login", "checkout/profile")
	h.do(t, 2)
	if sum := label(h, "checkout/login"); sum.Errors["extract_failed"] != 2 {
		t.Fatalf("login: %+v", sum)
	}
	if s.profileHits.Load() != 0 || label(h, "checkout/profile").N != 0 {
		t.Fatalf("the profile step ran %d times after a failed extraction", s.profileHits.Load())
	}
}

func TestJSONExpectations(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{"token":"abc","user":{"id":7}}`)
	yes, no := true, false
	for name, tc := range map[string]struct {
		checks []config.JSONExpect
		ok     bool
	}{
		"equals text":        {[]config.JSONExpect{{Path: "status", Equals: "ok"}}, true},
		"equals number":      {[]config.JSONExpect{{Path: "count", Equals: 3}}, true},
		"number as float":    {[]config.JSONExpect{{Path: "count", Equals: 3.0}}, true},
		"wrong value":        {[]config.JSONExpect{{Path: "status", Equals: "down"}}, false},
		"exists":             {[]config.JSONExpect{{Path: "count", Exists: &yes}}, true},
		"must not exist":     {[]config.JSONExpect{{Path: "error", Exists: &no}}, true},
		"missing path":       {[]config.JSONExpect{{Path: "missing", Exists: &yes}}, false},
		"number is not text": {[]config.JSONExpect{{Path: "count", Equals: "three"}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, checkout(s.URL, &config.Expect{JSON: tc.checks}, nil), "checkout/login", "checkout/profile")
			h.do(t, 1)
			got := label(h, "checkout/profile")
			if (got.OK == 1) != tc.ok || (!tc.ok && got.Errors["expect_failed"] != 1) {
				t.Fatalf("%+v", got)
			}
		})
	}
}

// Think time is the user's, not the target's: a later step is timed from when it
// started, so a pause before it never appears in its latency.
func TestThinkTimeIsExcludedFromLatency(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{"token":"abc","user":{"id":7}}`)
	h := newHarness(t, checkout(s.URL, nil, &config.ThinkTime{Type: "constant", Duration: dur(300 * time.Millisecond)}),
		"checkout/login", "checkout/profile")
	start := time.Now()
	h.do(t, 2)
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("two iterations with a 300ms think took %s: the pause did not happen", elapsed)
	}
	if p99 := label(h, "checkout/profile").Response.Max; p99 >= 250 {
		t.Fatalf("profile response max %.1fms includes the 300ms think time", p99)
	}
}

// Under the closed model a user's cookie jar lasts the whole run, so its next
// iteration's first request carries the session; under the open model every
// arrival is a new user and starts empty.
func TestCookiesLastPerUserNotPerArrival(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{"token":"abc","user":{"id":7}}`)
	h := newHarness(t, checkout(s.URL, nil, nil), "checkout/login", "checkout/profile")
	sess := &runner.Session{VU: 0}
	for i := range 2 {
		now := h.deps.Elapsed()
		it := runner.Iteration{Index: int64(i), Intended: now, Dispatched: now, WorkerStart: now, Rand: rand.New(rand.NewPCG(1, 2)), Session: sess}
		if err := h.runner.Do(context.Background(), &it, h.collector); err != nil {
			t.Fatal(err)
		}
	}
	h.do(t, 1) // an arrival: no session
	s.mu.Lock()
	defer s.mu.Unlock()
	want := []string{"/login=", "/users/7=s1", "/login=s1", "/users/7=s1", "/login=", "/users/7=s1"}
	if strings.Join(s.cookiesSeen, " ") != strings.Join(want, " ") {
		t.Fatalf("cookies seen %v, want %v", s.cookiesSeen, want)
	}
}

// pick: per-vu pins a user to its first weighted choice.
func TestPerVUPinsTheJourney(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{}`)
	one, many := 1.0, 1.0
	cfg := &config.HTTP{
		BaseURL: s.URL, Executor: config.Executor{Type: "vus", VUs: 1},
		Journeys: []config.Journey{
			{Name: "a", Weight: &one, Steps: []config.HTTPStep{{Name: "s", URL: "/a"}}},
			{Name: "b", Weight: &many, Steps: []config.HTTPStep{{Name: "s", URL: "/b"}}},
		},
	}
	h := newHarness(t, cfg, "a/s", "b/s")
	sess := &runner.Session{PerVU: true}
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range 20 {
		now := h.deps.Elapsed()
		it := runner.Iteration{Index: int64(i), Intended: now, Dispatched: now, WorkerStart: now, Rand: rng, Session: sess}
		if err := h.runner.Do(context.Background(), &it, h.collector); err != nil {
			t.Fatal(err)
		}
	}
	h.collector.Finish(time.Hour)
	a, b := label(h, "a/s").N, label(h, "b/s").N
	if a+b != 20 || (a != 0 && b != 0) {
		t.Fatalf("a %d, b %d: a pinned user must run one journey only", a, b)
	}
}

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "users.csv")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFeeders(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
	}))
	defer srv.Close()
	csv := writeCSV(t, "id,email\n1,a@x\n2,b@x\n")
	for mode, want := range map[string]string{
		"sequential": "/u/1 /u/2 /u/1",
		"unique":     "/u/1 /u/2",
	} {
		mu.Lock()
		paths = nil
		mu.Unlock()
		cfg := &config.HTTP{
			BaseURL: srv.URL, Executor: config.Executor{Type: "arrival-rate", Rate: 1},
			Feeders:  []config.Feeder{{Name: "users", File: csv, Mode: mode}},
			Requests: []config.HTTPStep{{Name: "r", URL: "/u/{{users.id}}"}},
		}
		h := newHarness(t, cfg, "r")
		h.do(t, 3)
		mu.Lock()
		got := strings.Join(paths, " ")
		mu.Unlock()
		if got != want {
			t.Fatalf("%s: paths %s, want %s", mode, got, want)
		}
		if mode == "unique" && label(h, "r").Errors["extract_failed"] != 1 {
			t.Fatalf("an exhausted unique feeder: %+v", label(h, "r"))
		}
	}

	// A column the file does not have is a configuration error, before any load.
	cfg := &config.HTTP{
		BaseURL: srv.URL, Executor: config.Executor{Type: "arrival-rate", Rate: 1},
		Feeders:  []config.Feeder{{Name: "users", File: csv}},
		Requests: []config.HTTPStep{{Name: "r", URL: "/u/{{users.phone}}"}},
	}
	if _, err := newRunner(t, cfg); codeOf(err) != errs.CodeConfigFeederNotFound {
		t.Fatalf("a missing column: %v", err)
	}
}

// Preflight walks each journey, so an extraction path that does not match what the
// target returns is caught before any load.
func TestPreflightCatchesABadExtractionPath(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{"token":"abc","uid":7}`)
	r, err := newRunner(t, checkout(s.URL, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	err = r.Prepare(context.Background())
	if codeOf(err) != errs.CodePreflightHTTP || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("preflight: %v", err)
	}
}

// With traceparent on, every step of an iteration carries one trace id and its own
// span, in W3C form.
func TestTraceparent(t *testing.T) {
	t.Parallel()
	s := newShop(t, `{"token":"abc","user":{"id":7}}`)
	cfg := checkout(s.URL, nil, nil)
	cfg.Traceparent = true
	h := newHarness(t, cfg, "checkout/login", "checkout/profile")
	h.do(t, 2)
	s.mu.Lock()
	defer s.mu.Unlock()
	re := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-01$`)
	var ids, spans []string
	for _, tp := range s.traces {
		m := re.FindStringSubmatch(tp)
		if m == nil {
			t.Fatalf("traceparent %q", tp)
		}
		ids, spans = append(ids, m[1]), append(spans, m[2])
	}
	if ids[0] != ids[1] || ids[2] != ids[3] || ids[0] == ids[2] || spans[0] == spans[1] {
		t.Fatalf("traces %v spans %v", ids, spans)
	}
}

// newRunner builds a runner with a collector that knows every label the config has.
func newRunner(t *testing.T, cfg *config.HTTP) (*httprun.Runner, error) {
	t.Helper()
	var labels []string
	for _, q := range cfg.Requests {
		labels = append(labels, q.Name)
	}
	for _, j := range cfg.Journeys {
		for _, st := range j.Steps {
			labels = append(labels, j.Name+"/"+st.Name)
		}
	}
	col, err := metrics.NewCollector(metrics.Config{Runner: "http", Labels: labels, BucketWidth: time.Second, MinSamples: 1})
	if err != nil {
		t.Fatal(err)
	}
	return httprun.New(cfg, time.Second, time.Second, runner.Deps{Collector: col, Clock: clock.New(), Start: time.Now(), Seed: 7})
}

func codeOf(err error) errs.Code {
	var typed *errs.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}
