// Package live is the terminal view of a running test, shown only when a person is
// watching a terminal (§5.8). Everywhere else - a pipe, a log file, CI, an agent -
// the run reports one log line every five seconds instead.
//
// The model is pure: Update folds messages into state and View draws it at a given
// width, so the view is tested without a terminal. Nothing here is analysis. The
// figures are the engine's own progress and each sealed bucket's p99, which is what a
// person watching wants: how far along, how fast, and whether latency is moving now.
package live

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/IshaanNene/Tracepoint/internal/engine"
	"github.com/IshaanNene/Tracepoint/internal/render"
)

// trendLen is how many sealed buckets the trend column shows.
const trendLen = 24

// maxNotes bounds the warnings kept on screen.
const maxNotes = 4

// Messages the model understands.
type (
	// RunMsg names the run once it has an id.
	RunMsg struct {
		ID    string
		Total time.Duration
		// Trail is how far latency figures lag the run: a bucket seals only once no
		// operation belonging to it can still complete, after the operation timeout.
		Trail time.Duration
	}
	// ProgressMsg is one runner's live counters.
	ProgressMsg engine.Progress
	// BucketMsg is one sealed bucket.
	BucketMsg struct {
		Runner       string
		P99MS        float64
		RPS          float64
		ErrorRatio   float64
		Insufficient bool
		Warmup       bool
	}
	// NoteMsg is a warning worth showing.
	NoteMsg struct{ Text string }
	// StoppingMsg says a stop was requested and the run is draining.
	StoppingMsg struct{}
	// DoneMsg ends the view.
	DoneMsg struct{ Status string }
)

type runnerState struct {
	progress engine.Progress
	seen     bool
	trend    []float64 // NaN where a bucket had too few samples
	last     BucketMsg
	sealed   int
}

// Model is the live view's state.
type Model struct {
	id       string
	total    time.Duration
	trail    time.Duration
	elapsed  time.Duration
	order    []string
	runners  map[string]*runnerState
	notes    []string
	width    int
	stopping bool
	done     string
	styles   styles
}

type styles struct {
	title, dim, bad, warn, bar, head lipgloss.Style
}

// New builds a model. r decides colour: pass a renderer with an ASCII profile for
// plain output.
func New(r *lipgloss.Renderer) Model {
	return Model{
		runners: map[string]*runnerState{},
		width:   80,
		styles: styles{
			title: r.NewStyle().Bold(true),
			dim:   r.NewStyle().Faint(true),
			bad:   r.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
			warn:  r.NewStyle().Foreground(lipgloss.Color("3")),
			bar:   r.NewStyle().Foreground(lipgloss.Color("6")),
			head:  r.NewStyle().Faint(true).Underline(true),
		},
	}
}

// Init starts nothing: every message arrives from the run.
func (m Model) Init() tea.Cmd { return nil }

// Update folds a message into the state.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
	case RunMsg:
		m.id, m.total, m.trail = msg.ID, msg.Total, msg.Trail
	case ProgressMsg:
		rs := m.runner(msg.Runner)
		rs.progress, rs.seen = engine.Progress(msg), true
		if msg.Elapsed > m.elapsed {
			m.elapsed = msg.Elapsed
		}
		if msg.Total > 0 {
			m.total = msg.Total
		}
	case BucketMsg:
		rs := m.runner(msg.Runner)
		rs.last = msg
		rs.sealed++
		v := msg.P99MS
		if msg.Insufficient {
			v = math.NaN()
		}
		rs.trend = append(rs.trend, v)
		if len(rs.trend) > trendLen {
			rs.trend = rs.trend[len(rs.trend)-trendLen:]
		}
	case NoteMsg:
		m.notes = append(m.notes, msg.Text)
		if len(m.notes) > maxNotes {
			m.notes = m.notes[len(m.notes)-maxNotes:]
		}
	case StoppingMsg:
		m.stopping = true
	case DoneMsg:
		m.done = msg.Status
		if m.done == "" {
			m.done = "finished"
		}
		return m, tea.Quit
	}
	return m, nil
}

// runner returns a runner's state, creating it in first-seen order. Callers hold the
// model by value, but the map is shared, which is what Bubble Tea's single update
// goroutine expects.
func (m *Model) runner(name string) *runnerState {
	rs, ok := m.runners[name]
	if !ok {
		rs = &runnerState{}
		m.runners[name] = rs
		m.order = append(m.order, name)
	}
	return rs
}

// View draws the state.
func (m Model) View() string {
	s := m.styles
	b := &strings.Builder{}

	title := "tracepoint"
	if m.id != "" {
		title += "  " + m.id
	}
	b.WriteString(s.title.Render(title) + "\n")

	ratio := 0.0
	if m.total > 0 {
		ratio = math.Min(1, float64(m.elapsed)/float64(m.total))
	}
	barWidth := max(10, min(40, m.width-30))
	filled := int(math.Round(ratio * float64(barWidth)))
	bar := s.bar.Render(strings.Repeat("█", filled)) + s.dim.Render(strings.Repeat("░", barWidth-filled))
	fmt.Fprintf(b, "%s  %s / %s  %3.0f%%\n\n", bar, clock(m.elapsed), clock(m.total), ratio*100)

	rows := [][]string{{"runner", "in flight", "done", "errors", "rps", "p99", "trend"}}
	for _, name := range m.order {
		rs := m.runners[name]
		errsCell := "—"
		if rs.seen {
			ratio := 0.0
			if rs.progress.Done > 0 {
				ratio = float64(rs.progress.Errors) / float64(rs.progress.Done)
			}
			errsCell = fmt.Sprintf("%d (%s)", rs.progress.Errors, render.Percent(ratio))
		}
		p99, rps := "—", "—"
		if rs.sealed > 0 {
			rps = render.Number(math.Round(rs.last.RPS))
			if rs.last.Insufficient {
				p99 = "too few"
			} else {
				p99 = render.MS(rs.last.P99MS)
			}
		}
		rows = append(rows, []string{
			name, fmt.Sprint(rs.progress.InFlight), fmt.Sprint(rs.progress.Done), errsCell, rps, p99, Sparkline(rs.trend),
		})
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, c := range row {
			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}
	for r, row := range rows {
		line := &strings.Builder{}
		for i, c := range row {
			cell := c + strings.Repeat(" ", widths[i]-lipgloss.Width(c))
			switch {
			case r == 0:
				cell = s.head.Render(c) + strings.Repeat(" ", widths[i]-lipgloss.Width(c))
			case i == 3 && m.runners[row[0]].progress.Errors > 0:
				cell = s.bad.Render(cell)
			}
			line.WriteString(cell)
			if i < len(row)-1 {
				line.WriteString("  ")
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	if len(m.order) == 0 {
		b.WriteString(s.dim.Render("preflight: resolving targets and connecting") + "\n")
	} else if m.trail > 0 {
		b.WriteString(s.dim.Render(fmt.Sprintf("errors, rps and p99 are per sealed bucket and trail by %s, the operation timeout", render.Seconds(m.trail.Seconds()))) + "\n")
	}

	if len(m.notes) > 0 {
		b.WriteString("\n")
		for _, n := range m.notes {
			b.WriteString(s.warn.Render("! "+truncate(n, m.width-2)) + "\n")
		}
	}
	b.WriteString("\n")
	switch {
	case m.done != "":
		b.WriteString(s.dim.Render("run "+m.done+"; analysing and writing the report") + "\n")
	case m.stopping:
		b.WriteString(s.warn.Render("stopping: in-flight work is draining and a partial result will be written; interrupt again to exit now") + "\n")
	default:
		b.WriteString(s.dim.Render("ctrl+c stops gracefully and writes a partial result") + "\n")
	}
	return b.String()
}

var ticks = []rune("▁▂▃▄▅▆▇█")

// Sparkline draws values scaled between their own minimum and maximum; a NaN - a bucket
// with too few samples - is a gap, drawn as a dot.
func Sparkline(vs []float64) string {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vs {
		if !math.IsNaN(v) {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	out := make([]rune, len(vs))
	for i, v := range vs {
		switch {
		case math.IsNaN(v):
			out[i] = '·'
		case hi <= lo:
			out[i] = ticks[0]
		default:
			out[i] = ticks[int(math.Round((v-lo)/(hi-lo)*float64(len(ticks)-1)))]
		}
	}
	return string(out)
}

func clock(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func truncate(s string, n int) string {
	if n < 4 || len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
