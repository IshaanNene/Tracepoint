// Package events defines the run event stream (schemas/events.schema.json) and the
// emitter that numbers events and fans them out to every sink - events.ndjson in the
// run directory, stdout under --events -, and an in-process callback.
//
// The engine emits; it does not know where events go. That keeps the engine free of
// file handling and lets the same stream reach a file, a pipe and a library caller.
package events

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
)

// Version is the event envelope version.
const Version = 1

// Event types. New types are added in minor versions; readers ignore unknown ones.
const (
	RunStarted         = "run.started"
	PreflightCompleted = "preflight.completed"
	BucketSealed       = "bucket.sealed"
	IncidentOpened     = "incident.opened"
	IncidentClosed     = "incident.closed"
	LevelStarted       = "level.started"
	LevelCompleted     = "level.completed"
	Warning            = "warning"
	RunFinished        = "run.finished"
)

// Event is one line of the stream.
type Event struct {
	V     int     `json:"v"`
	Seq   int64   `json:"seq"`
	TMS   float64 `json:"t_ms"`
	Wall  string  `json:"wall,omitempty"`
	RunID string  `json:"run_id"`
	Type  string  `json:"type"`
	Data  any     `json:"data,omitempty"`
}

// Emitter numbers events and delivers them to its sinks, in order, one at a time.
// It is safe for concurrent use.
type Emitter struct {
	mu      sync.Mutex
	runID   string
	clk     clock.Clock
	start   time.Time
	seq     int64
	writers []io.Writer
	funcs   []func(Event)
	err     error
}

// NewEmitter builds an emitter for one run. Offsets are measured from start once it
// is set with SetStart; before then they are negative-free zeros, since preflight has
// no run clock yet.
func NewEmitter(runID string, clk clock.Clock) *Emitter {
	if clk == nil {
		clk = clock.New()
	}
	return &Emitter{runID: runID, clk: clk}
}

// SetStart fixes the run's monotonic origin, the one every runner measures from.
func (e *Emitter) SetStart(t time.Time) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.start = t
	e.mu.Unlock()
}

// AddWriter adds a sink that receives each event as one NDJSON line.
func (e *Emitter) AddWriter(w io.Writer) {
	e.mu.Lock()
	e.writers = append(e.writers, w)
	e.mu.Unlock()
}

// AddFunc adds a sink that receives each event as a value.
func (e *Emitter) AddFunc(f func(Event)) {
	e.mu.Lock()
	e.funcs = append(e.funcs, f)
	e.mu.Unlock()
}

// Emit numbers an event and delivers it. A write failure is remembered and reported
// by Err rather than stopping the run: losing a progress line must never lose the
// measurement.
func (e *Emitter) Emit(typ string, data any) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	e.seq++
	ev := Event{V: Version, Seq: e.seq, RunID: e.runID, Type: typ, Data: data,
		Wall: now.UTC().Format(time.RFC3339Nano)}
	if !e.start.IsZero() {
		ev.TMS = round3(float64(now.Sub(e.start)) / float64(time.Millisecond))
	}
	if len(e.writers) > 0 {
		line, err := json.Marshal(ev)
		if err != nil {
			e.remember(fmt.Errorf("encoding a %s event: %w", typ, err))
		} else {
			line = append(line, '\n')
			for _, w := range e.writers {
				if _, werr := w.Write(line); werr != nil {
					e.remember(fmt.Errorf("writing a %s event: %w", typ, werr))
				}
			}
		}
	}
	for _, f := range e.funcs {
		f(ev)
	}
}

func (e *Emitter) remember(err error) {
	if e.err == nil {
		e.err = err
	}
}

// Err reports the first delivery failure, if any.
func (e *Emitter) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// Seq is the number of events emitted so far.
func (e *Emitter) Seq() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq
}

func round3(v float64) float64 {
	if v < 0 {
		return -round3(-v)
	}
	return float64(int64(v*1000+0.5)) / 1000
}
