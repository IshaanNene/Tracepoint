package engine

import (
	"time"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/events"
)

func (e *Engine) emitPreflight() {
	if e.opts.Events == nil {
		return
	}
	targets := map[string][]string{}
	for _, br := range e.runners {
		for _, t := range br.targets {
			targets[br.name] = append(targets[br.name], t.Host)
		}
	}
	e.opts.Events.Emit(events.PreflightCompleted, map[string]any{"targets": targets})
	for _, w := range e.opts.Warnings {
		e.opts.Events.Emit(events.Warning, map[string]any{"code": w.Code, "message": w.Message, "severity": w.Severity})
	}
}

func (e *Engine) emitStarted() {
	if e.opts.Events == nil {
		return
	}
	cfg := e.opts.Config
	runners := make([]map[string]string, 0, len(e.runners))
	for _, br := range e.runners {
		runners = append(runners, map[string]string{"name": br.name, "kind": config.Kind(br.name)})
	}
	e.opts.Events.Emit(events.RunStarted, map[string]any{
		"duration_ms": ms(cfg.Run.Duration.D()),
		"bucket_ms":   ms(cfg.Run.Bucket.D()),
		"warmup_ms":   ms(cfg.Run.Warmup.D()),
		"seed":        e.seed,
		"runners":     runners,
	})
}

// emitSealed reports every bucket sealed since the last call. It runs on the sweeper
// goroutine and once more after the final flush, never concurrently.
func (e *Engine) emitSealed() {
	if e.opts.Events == nil {
		return
	}
	for _, br := range e.runners {
		for _, b := range br.collector.SealedAfter(br.lastSealed) {
			br.lastSealed = int64(b.Index)
			data := map[string]any{
				"runner": br.name, "index": b.Index, "n": b.N,
				"response_p99_ms": round3(b.Response.P99), "service_p99_ms": round3(b.Service.P99),
				"rps": round3(b.RPS),
			}
			if b.N > 0 {
				data["error_ratio"] = round3(float64(b.ErrorsTotal) / float64(b.N))
			}
			if b.Insufficient {
				data["insufficient"] = true
			}
			if b.Warmup {
				data["warmup"] = true
			}
			e.opts.Events.Emit(events.BucketSealed, data)
		}
	}
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
