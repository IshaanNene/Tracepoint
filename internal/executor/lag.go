package executor

import (
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/DataDog/sketches-go/ddsketch/mapping"
	"github.com/DataDog/sketches-go/ddsketch/store"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
)

// lagTracker measures how late arrivals were handed out relative to their scheduled
// time.
//
// This is the single most important number for deciding whether a run can be believed
// at all: if the generator could not keep to its own schedule, every latency it
// reports includes that delay, and the run describes us rather than the target
// (spec §5.5).
type lagTracker struct {
	sketch *ddsketch.DDSketch
	n      int64
	sum    float64
	max    float64
}

func newLagTracker() (*lagTracker, error) {
	m, err := mapping.NewLogarithmicMapping(metrics.DefaultRelativeAccuracy)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "building the dispatch-lag sketch")
	}
	return &lagTracker{sketch: ddsketch.NewDDSketch(m, store.NewDenseStore(), store.NewDenseStore())}, nil
}

func (l *lagTracker) add(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	if l.sketch.Add(ms) != nil {
		return
	}
	l.n++
	l.sum += ms
	if ms > l.max {
		l.max = ms
	}
}

func (l *lagTracker) quantiles() metrics.Quantiles {
	q := metrics.Quantiles{Max: l.max}
	if l.n > 0 {
		q.Mean = l.sum / float64(l.n)
	}
	if l.sketch.GetCount() == 0 {
		return q
	}
	read := func(p float64) float64 {
		v, err := l.sketch.GetValueAtQuantile(p)
		if err != nil || v < 0 {
			return 0
		}
		return v
	}
	q.P50, q.P90, q.P95, q.P99, q.P999 = read(0.5), read(0.9), read(0.95), read(0.99), read(0.999)
	return q
}
