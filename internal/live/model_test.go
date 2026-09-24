package live

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/engine"
	"github.com/IshaanNene/Tracepoint/internal/events"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func plain() Model {
	r := lipgloss.NewRenderer(&bytes.Buffer{})
	r.SetColorProfile(termenv.Ascii)
	return New(r)
}

func step(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	return m
}

func TestViewShowsProgressRunnersAndTrend(t *testing.T) {
	m := step(t, plain(),
		tea.WindowSizeMsg{Width: 100},
		RunMsg{ID: "20260924T100000Z-abc123", Total: 30 * time.Second, Trail: 10 * time.Second},
		ProgressMsg{Runner: "http", Elapsed: 15 * time.Second, Done: 1500, Errors: 15, InFlight: 4},
		ProgressMsg{Runner: "db", Elapsed: 15 * time.Second, Done: 150},
		BucketMsg{Runner: "http", P99MS: 20, RPS: 100},
		BucketMsg{Runner: "http", P99MS: 900, RPS: 99},
		BucketMsg{Runner: "db", Insufficient: true},
		NoteMsg{Text: "SAME_HOST the generator and a target share a host"},
	)
	out := m.View()
	for _, want := range []string{
		"tracepoint  20260924T100000Z-abc123", "0:15 / 0:30   50%",
		"http", "15 (1.0%)", "900ms", "▁█",
		"db", "too few", "·",
		"! SAME_HOST the generator", "ctrl+c stops gracefully", "trail by 10s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "http") > strings.Index(out, "\ndb") {
		t.Error("runners must keep the order they were first seen in")
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("an ASCII renderer must not emit escapes")
	}
}

func TestStoppingAndDone(t *testing.T) {
	m := step(t, plain(), StoppingMsg{})
	if !strings.Contains(m.View(), "interrupt again to exit now") {
		t.Fatal("no stopping notice")
	}
	next, cmd := m.Update(DoneMsg{Status: "interrupted"})
	if cmd == nil || !strings.Contains(next.View(), "run interrupted") {
		t.Fatal("done must quit and say how the run ended")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("done must return tea.Quit")
	}
}

func TestNotesAndTrendAreBounded(t *testing.T) {
	m := plain()
	for i := range 40 {
		m = step(t, m, BucketMsg{Runner: "http", P99MS: float64(i)}, NoteMsg{Text: "note"})
	}
	if n := len(m.runners["http"].trend); n != trendLen {
		t.Fatalf("trend holds %d, want %d", n, trendLen)
	}
	if len(m.notes) != maxNotes {
		t.Fatalf("%d notes kept", len(m.notes))
	}
}

func TestSparkline(t *testing.T) {
	if got := Sparkline([]float64{1, 5, math.NaN(), 9}); got != "▁▅·█" {
		t.Fatalf("got %q", got)
	}
	if got := Sparkline([]float64{3, 3}); got != "▁▁" {
		t.Fatalf("a flat line: %q", got)
	}
	if Sparkline(nil) != "" {
		t.Fatal("empty")
	}
}

// The driver, run against a buffer: events and log records become messages, and
// Finish releases the terminal and leaves nothing running.
func TestViewDriver(t *testing.T) {
	var out bytes.Buffer
	v := Start(&out, false)
	v.Run("run-1", 2*time.Second, time.Second)
	v.Progress(engine.Progress{Runner: "http", Elapsed: time.Second, Done: 10})
	v.Event(events.Event{Type: events.BucketSealed, Data: map[string]any{"runner": "http", "response_p99_ms": 12.0, "rps": 10.0}})
	v.Event(events.Event{Type: events.Warning, Data: map[string]any{"code": "W1", "message": "watch out"}})
	log := slog.New(v.Handler())
	log.Info("hidden while the view owns the terminal")
	log.Warn("visible", "code", "W2")
	if log.Handler().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info must not reach the view")
	}
	v.Finish("completed")
	got := out.String()
	for _, want := range []string{"run-1", "W1 watch out", "W2 visible", "run completed"} {
		if !strings.Contains(got, want) {
			t.Errorf("final output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hidden while") {
		t.Error("an info record tore the view")
	}
}
