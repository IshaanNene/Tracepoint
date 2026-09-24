package events_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/events"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
	"github.com/IshaanNene/Tracepoint/internal/schemas/schematest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestEmitterNumbersAndFansOut(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	e := events.NewEmitter("run-1", clk)
	var a, b bytes.Buffer
	e.AddWriter(&a)
	e.AddWriter(&b)
	var got []events.Event
	e.AddFunc(func(ev events.Event) { got = append(got, ev) })

	e.Emit(events.PreflightCompleted, map[string]any{})
	e.SetStart(clk.Now())
	clk.Advance(1500 * time.Millisecond)
	e.Emit(events.BucketSealed, map[string]any{"runner": "http", "index": 0, "n": 12})

	if a.String() != b.String() || strings.Count(a.String(), "\n") != 2 {
		t.Fatalf("sinks differ or miscount:\n%s\n%s", a.String(), b.String())
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 || got[1].TMS != 1500 || got[0].TMS != 0 {
		t.Fatalf("events = %+v", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(a.String()), "\n") {
		schematest.Validate(t, schemas.Events, []byte(line))
	}
}

// A sink that fails is remembered, not fatal: losing a progress line must never lose
// the measurement.
func TestEmitterSurvivesAFailingSink(t *testing.T) {
	e := events.NewEmitter("r", nil)
	e.AddWriter(failing{})
	var ok bytes.Buffer
	e.AddWriter(&ok)
	e.Emit(events.Warning, map[string]any{"code": "X", "message": "m"})
	if e.Err() == nil || ok.Len() == 0 {
		t.Fatalf("err=%v, delivered=%d", e.Err(), ok.Len())
	}
}

func TestEmitterIsSafeConcurrentlyAndNilSafe(t *testing.T) {
	var nilEmitter *events.Emitter
	nilEmitter.Emit(events.Warning, nil)
	nilEmitter.SetStart(time.Now())

	e := events.NewEmitter("r", nil)
	var buf safeBuffer
	e.AddWriter(&buf)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				e.Emit(events.Warning, map[string]any{"code": "X", "message": "m"})
			}
		}()
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev events.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("a torn line: %q", line)
		}
		seen[ev.Seq] = true
	}
	if len(seen) != 400 || e.Seq() != 400 {
		t.Fatalf("%d distinct seqs, seq %d", len(seen), e.Seq())
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
