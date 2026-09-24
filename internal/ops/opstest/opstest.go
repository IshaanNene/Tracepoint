// Package opstest builds an operation service for adapter tests: runs execute in the
// test process against a local HTTP target, so a test of the MCP or REST transport
// can drive a whole agent loop in a couple of seconds.
package opstest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/session"
)

// Harness is a service and the target its configurations load.
type Harness struct {
	Service *ops.Service
	Target  *httptest.Server
	Dir     string
	wg      sync.WaitGroup
}

// New builds a harness whose resources are released when the test ends, after every
// run it launched has finished.
func New(t testing.TB) *Harness {
	t.Helper()
	h := &Harness{Dir: t.TempDir()}
	h.Target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
			return
		}
	}))
	store, err := runstore.Open(filepath.Join(h.Dir, "runs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Service = &ops.Service{
		Policy: policy.ServerDefault(), Store: store, Root: h.Dir, Actor: "test",
		Lookup: func(string) (string, bool) { return "", false },
		// A launched run outlives the call that started it, as a detached one does.
		Launch: func(ctx context.Context, p *session.Prepared) (int, error) {
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				if _, err := p.Execute(context.WithoutCancel(ctx)); err != nil {
					t.Logf("run %s: %v", p.ID(), err)
				}
			}()
			return os.Getpid(), nil
		},
	}
	t.Cleanup(func() {
		h.wg.Wait()
		h.Target.Close()
	})
	return h
}

// Config is a small valid configuration against the target.
func (h *Harness) Config(duration string) string {
	return fmt.Sprintf(`
version: 1
run: { duration: %s, bucket: 500ms, seed: 3, timeout: 2s }
slo: { http: { p99: 2s } }
http:
  base_url: %q
  executor: { rate: 20, max_in_flight: 8 }
  requests: [{ name: items, url: "/api/items" }]
`, duration, h.Target.URL)
}
