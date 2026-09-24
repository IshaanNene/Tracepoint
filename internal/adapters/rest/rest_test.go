package rest_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/adapters/guard"
	"github.com/IshaanNene/Tracepoint/internal/adapters/rest"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/ops/opstest"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const token = "0123456789abcdef0123"

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}

type server struct {
	h    *opstest.Harness
	srv  *httptest.Server
	host string
}

func newServer(t *testing.T) *server {
	t.Helper()
	h := opstest.New(t)
	srv := httptest.NewUnstartedServer(nil)
	addr := srv.Listener.Addr().String()
	g, err := guard.New(addr, guard.Options{Token: token, Open: []string{rest.HealthPath}})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = g.Wrap(rest.Handler(h.Service))
	srv.Start()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return &server{h: h, srv: srv, host: addr}
}

// do sends a request; mutate adjusts it after the token and Host are set.
func (s *server) do(t *testing.T, method, path string, body any, mutate func(*http.Request)) (status int, _ []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	resp, err := s.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%s %s answered with a CORS header %s", method, path, k)
		}
	}
	return resp.StatusCode, out
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	schematest.Validate(t, schemas.Error, body)
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return env.Error.Code
}

// Every check of §6.5, each on its own.
func TestGuard(t *testing.T) {
	s := newServer(t)
	for name, tc := range map[string]struct {
		method, path string
		mutate       func(*http.Request)
		status       int
		code         string
	}{
		"no token":      {"POST", rest.Prefix + "get_policy", func(r *http.Request) { r.Header.Del("Authorization") }, 401, "SERVER_UNAUTHORIZED"},
		"wrong token":   {"POST", rest.Prefix + "get_policy", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 20)) }, 401, "SERVER_UNAUTHORIZED"},
		"basic auth":    {"POST", rest.Prefix + "get_policy", func(r *http.Request) { r.SetBasicAuth("a", token) }, 401, "SERVER_UNAUTHORIZED"},
		"foreign host":  {"POST", rest.Prefix + "get_policy", func(r *http.Request) { r.Host = "evil.example:80" }, 421, "SERVER_BAD_HOST"},
		"an origin":     {"POST", rest.Prefix + "get_policy", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403, "SERVER_BAD_ORIGIN"},
		"a preflight":   {"OPTIONS", rest.Prefix + "get_policy", nil, 403, "SERVER_BAD_ORIGIN"},
		"unknown route": {"POST", rest.Prefix + "launch_missiles", nil, 404, "OPS_UNKNOWN_OPERATION"},
		"wrong method":  {"GET", rest.Prefix + "get_policy", nil, 404, "OPS_UNKNOWN_OPERATION"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(t, tc.method, tc.path, map[string]any{}, tc.mutate)
			if status != tc.status {
				t.Fatalf("status %d, want %d: %s", status, tc.status, body)
			}
			if got := errCode(t, body); got != tc.code {
				t.Fatalf("code %s, want %s", got, tc.code)
			}
		})
	}

	// The health check needs no token but is still held to the Host check.
	if status, _ := s.do(t, "GET", rest.HealthPath, nil, func(r *http.Request) { r.Header.Del("Authorization") }); status != 200 {
		t.Fatalf("health: %d", status)
	}
	if status, _ := s.do(t, "GET", rest.HealthPath, nil, func(r *http.Request) { r.Host = "evil.example" }); status != 421 {
		t.Fatalf("health on a foreign host: %d", status)
	}
	// Loopback aliases of the listen address are the same server.
	_, port, _ := strings.Cut(s.host, ":")
	if status, _ := s.do(t, "POST", rest.Prefix+"get_policy", map[string]any{}, func(r *http.Request) { r.Host = "localhost:" + port }); status != 200 {
		t.Fatalf("localhost alias: %d", status)
	}
}

// A whole agent loop over REST, with statuses that match the error classes.
func TestAgentLoopOverREST(t *testing.T) {
	s := newServer(t)
	cfg := s.h.Config("2s")

	status, body := s.do(t, "POST", rest.Prefix+"validate_config", map[string]any{"config": cfg}, nil)
	if status != 200 || !strings.Contains(string(body), `"valid":true`) {
		t.Fatalf("validate: %d %s", status, body)
	}
	if st, b := s.do(t, "POST", rest.Prefix+"plan_run", map[string]any{"config": cfg}, nil); st != 200 {
		t.Fatalf("plan: %d %s", st, b)
	}
	status, body = s.do(t, "POST", rest.Prefix+"start_run", map[string]any{"config": cfg}, nil)
	if status != 200 {
		t.Fatalf("start: %d %s", status, body)
	}
	var started ops.StartOut
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	if st, b := s.do(t, "POST", rest.Prefix+"start_run", map[string]any{"config": cfg}, nil); st != 409 || errCode(t, b) != string(errs.CodePolicyTooManyRuns) {
		t.Fatalf("a concurrent run: %d %s", st, b)
	}

	var waited ops.WaitOut
	for deadline := time.Now().Add(30 * time.Second); !waited.Finished && time.Now().Before(deadline); {
		_, b := s.do(t, "POST", rest.Prefix+"wait_for_run", map[string]any{"run_id": started.RunID, "timeout_s": 5}, nil)
		if err := json.Unmarshal(b, &waited); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
	}
	if !waited.Finished || *waited.ExitCode != 0 {
		t.Fatalf("wait = %+v", waited)
	}
	status, body = s.do(t, "POST", rest.Prefix+"get_run_digest", map[string]any{"run_id": started.RunID}, nil)
	if status != 200 {
		t.Fatalf("digest: %d %s", status, body)
	}
	schematest.Validate(t, schemas.Digest, body)

	for name, tc := range map[string]struct {
		op     string
		in     any
		status int
	}{
		"unknown run":   {"get_run_status", map[string]any{"run_id": "nope"}, 404},
		"unknown field": {"get_run_status", map[string]any{"run_id": "x", "surprise": 1}, 400},
		"bad config":    {"validate_config", map[string]any{"config": "version: 1\nhttp: {"}, 400},
		"escaping path": {"validate_config", map[string]any{"config_path": "../../etc/passwd"}, 400},
	} {
		if st, b := s.do(t, "POST", rest.Prefix+tc.op, tc.in, nil); st != tc.status {
			t.Errorf("%s: %d, want %d: %s", name, st, tc.status, b)
		}
	}
	status, _ = s.do(t, "POST", rest.Prefix+"get_policy", strings.Repeat("x", 2<<20), nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body: %d", status)
	}

	audit, err := os.ReadFile(s.h.Service.Store.AuditPath())
	if err != nil || !strings.Contains(string(audit), `"rest"`) {
		t.Fatalf("audit log: %v\n%s", err, audit)
	}
}

// The OpenAPI document is generated from the registry and recorded.
func TestOpenAPI(t *testing.T) {
	s := newServer(t)
	status, body := s.do(t, "GET", rest.OpenAPIPath, nil, nil)
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" || len(doc.Paths) != len(ops.Registry()) {
		t.Fatalf("openapi %s with %d paths", doc.OpenAPI, len(doc.Paths))
	}
	for _, op := range ops.Registry() {
		if doc.Paths[rest.Prefix+op.Name]["post"] == nil {
			t.Errorf("no path for %s", op.Name)
		}
	}
	pretty, err := json.MarshalIndent(rest.OpenAPI(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "openapi.golden.json")
	if *update {
		if werr := os.MkdirAll("testdata", 0o750); werr != nil {
			t.Fatal(werr)
		}
		if werr := os.WriteFile(path, append(pretty, '\n'), 0o600); werr != nil {
			t.Fatal(werr)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run `go test ./internal/adapters/rest/ -update` to create it", err)
	}
	if string(want) != string(pretty)+"\n" {
		t.Errorf("%s changed; review and rerun with -update if intended", path)
	}
}
