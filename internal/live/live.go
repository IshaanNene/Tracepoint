package live

import (
	"context"
	"io"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/IshaanNene/Tracepoint/internal/engine"
	"github.com/IshaanNene/Tracepoint/internal/events"
)

// View runs the model on a terminal until Finish.
type View struct {
	program *tea.Program
	done    chan struct{}
	// err is why the view stopped drawing, if it failed; the run is unaffected.
	err error
}

// Start draws the live view on out, which must be a terminal. It takes no input: the
// terminal stays in its normal mode, so ctrl+c is still the signal the run already
// handles - the first stops gracefully, the second exits.
func Start(out io.Writer, colour bool) *View {
	r := lipgloss.NewRenderer(out)
	if !colour {
		r.SetColorProfile(termenv.Ascii)
	}
	v := &View{done: make(chan struct{})}
	v.program = tea.NewProgram(New(r), tea.WithOutput(out), tea.WithInput(nil), tea.WithoutSignalHandler())
	go func() {
		defer close(v.done)
		// A view that cannot draw leaves the run itself unaffected.
		if _, err := v.program.Run(); err != nil {
			v.err = err
		}
	}()
	return v
}

// Run names the run.
func (v *View) Run(id string, total, trail time.Duration) {
	v.program.Send(RunMsg{ID: id, Total: total, Trail: trail})
}

// Progress is an engine progress callback.
func (v *View) Progress(p engine.Progress) { v.program.Send(ProgressMsg(p)) }

// Event is an event-stream callback: sealed buckets feed the trend, warnings the
// notes.
func (v *View) Event(ev events.Event) {
	data, ok := ev.Data.(map[string]any)
	if !ok {
		return
	}
	switch ev.Type {
	case events.BucketSealed:
		msg := BucketMsg{
			Runner: str(data, "runner"), P99MS: num(data, "response_p99_ms"), RPS: num(data, "rps"),
			ErrorRatio: num(data, "error_ratio"), Insufficient: flag(data, "insufficient"), Warmup: flag(data, "warmup"),
		}
		if msg.Runner != "" {
			v.program.Send(msg)
		}
	case events.Warning:
		v.program.Send(NoteMsg{Text: str(data, "code") + " " + str(data, "message")})
	}
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func num(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

func flag(m map[string]any, k string) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return false
}

// Stopping says a stop was requested.
func (v *View) Stopping() { v.program.Send(StoppingMsg{}) }

// Err is why the view stopped drawing early, if it did.
func (v *View) Err() error {
	<-v.done
	return v.err
}

// Finish ends the view and waits until the terminal is released, so what is printed
// next lands below it.
func (v *View) Finish(status string) {
	v.program.Send(DoneMsg{Status: status})
	select {
	case <-v.done:
	case <-time.After(2 * time.Second):
		v.program.Kill()
		<-v.done
	}
}

// Handler shows log records at warn and above as notes, and drops the rest: while the
// view owns the terminal a log line would tear it. run.log still receives every
// record, because the session writes its own copy.
func (v *View) Handler() slog.Handler { return noteHandler{v: v} }

type noteHandler struct {
	v     *View
	attrs []slog.Attr
}

func (h noteHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h noteHandler) Handle(_ context.Context, r slog.Record) error {
	text := r.Message
	for _, a := range h.attrs {
		if a.Key == "code" {
			text = a.Value.String() + " " + text
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "code" {
			text = a.Value.String() + " " + text
		}
		return true
	})
	h.v.program.Send(NoteMsg{Text: text})
	return nil
}

func (h noteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return noteHandler{v: h.v, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h noteHandler) WithGroup(string) slog.Handler { return h }
