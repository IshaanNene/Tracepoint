package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/engine"
)

// One line every five seconds of run time, carrying every runner, written once the
// last runner of a round has reported.
func TestPlainProgress(t *testing.T) {
	var buf bytes.Buffer
	p := newPlainProgress(slog.New(slog.NewTextHandler(&buf, nil)))
	p.setRunners([]string{"http", "db"})
	for s := 1; s <= 11; s++ {
		for _, name := range []string{"http", "db"} {
			p.observe(engine.Progress{Runner: name, Elapsed: time.Duration(s) * time.Second, Total: 30 * time.Second, Done: int64(s * 10)})
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines in 11s, want 2:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "elapsed=5s") || !strings.Contains(lines[0], "http.done=50") || !strings.Contains(lines[0], "db.done=50") {
		t.Fatalf("line = %s", lines[0])
	}
	if !strings.Contains(lines[1], "elapsed=10s") {
		t.Fatalf("line = %s", lines[1])
	}
}

// The handler follows the switch, including for loggers derived before it.
func TestSwitchHandler(t *testing.T) {
	var a, b bytes.Buffer
	sw := newSwitchHandler(slog.NewTextHandler(&a, nil))
	log := slog.New(sw).With("run_id", "r1")
	log.Info("first")
	sw.set(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelWarn}))
	log.Info("hidden")
	log.Warn("second")
	if !strings.Contains(a.String(), "first") || !strings.Contains(a.String(), "run_id=r1") || strings.Contains(a.String(), "second") {
		t.Fatalf("a = %s", a.String())
	}
	if !strings.Contains(b.String(), "second") || !strings.Contains(b.String(), "run_id=r1") || strings.Contains(b.String(), "hidden") {
		t.Fatalf("b = %s", b.String())
	}
	if log.Handler().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled must follow the current handler")
	}
}
