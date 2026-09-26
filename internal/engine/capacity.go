package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/capacity"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/events"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/schedule"
)

// Finding codes a capacity search can add to its result.
const (
	CodeCapacityIncomplete = "CAPACITY_INCOMPLETE"
)

// capacityRun is what a search hands to the result builder.
type capacityRun struct {
	result   *result.Capacity
	warnings []result.Finding
}

// runSearch drives an auto-ramp capacity search (spec §5.6).
//
// Every level runs on the same runners, so connections stay warm, and on the run's one
// clock, so the whole search is a single timeline. A level begins on a bucket boundary
// and holds its value for step_duration; its first settle is discarded and the whole
// buckets after it are its measured window, whose figures come from sketches merged
// across those buckets. Once a level has drained nothing else is in flight, so its
// buckets are sealed at once rather than after the seal delay, and the next level
// starts after the cooldown, on a later bucket - two levels never share a bucket.
//
// run.duration is the search's time budget: a level that would not fit ends the
// search, and the result says so.
func (e *Engine) runSearch(arrivalCtx, workCtx context.Context) (*capacityRun, error) {
	cfg := e.opts.Config
	cp := cfg.Capacity
	plan, err := cp.Plan()
	if err != nil {
		return nil, err
	}
	search, err := capacity.NewSearch(plan)
	if err != nil {
		return nil, err
	}

	bw := cfg.Run.Bucket.D()
	step, settle, cooldown := cp.StepDuration.D(), cp.Settle.D(), cp.Cooldown.D()
	budget := cfg.Run.Duration.D()
	budgets := sloBudgets(cfg.SLO)

	out := &capacityRun{result: &result.Capacity{Knob: cp.Knob, Runner: cp.Runner, Levels: []result.CapacityLevel{}}}
	var below *capacity.Point
	var offset time.Duration
	incomplete := ""

	for level := 0; ; level++ {
		st, more := search.Next()
		if !more {
			break
		}
		if arrivalCtx.Err() != nil {
			incomplete = "the run was stopped before the search finished"
			break
		}
		if offset+step > budget {
			incomplete = fmt.Sprintf("run.duration (%s) ran out before the search finished", budget)
			break
		}

		from := int64((offset + settle + bw - 1) / bw)
		to := int64((offset + step) / bw)
		windows := make(map[string]int, len(e.runners))
		for _, br := range e.runners {
			id, werr := br.collector.OpenWindow(from, to)
			if werr != nil {
				return nil, werr
			}
			windows[br.name] = id
		}
		for _, br := range e.runners {
			if err := e.levelExecutor(br, cp, st.Value, step, offset); err != nil {
				return nil, err
			}
		}
		e.emitLevel(events.LevelStarted, level, cp.Knob, st, nil)

		// VUs start as soon as they are run, so wait for the level's start; arrival
		// schedules would wait for it anyway.
		if err := e.clk.SleepUntil(arrivalCtx, e.start.Add(offset)); err != nil {
			incomplete = "the run was stopped before the search finished"
			break
		}
		if err := e.drive(arrivalCtx, workCtx, step); err != nil {
			return nil, err
		}

		// The level has drained: nothing in flight belongs to any bucket up to now.
		now := e.clk.Now().Sub(e.start)
		last := int64(now / bw)
		// The sweeper reports them as sealed on its next tick; it alone emits them.
		for _, br := range e.runners {
			br.collector.SealBefore(last + 1)
		}

		lv := e.judgeLevel(level, st, cp, windows, budgets, below)
		lv.StartIndex, lv.EndIndex = int(from), int(to-1)
		out.result.Levels = append(out.result.Levels, lv.CapacityLevel)
		e.emitLevel(events.LevelCompleted, level, cp.Knob, st, &lv.CapacityLevel)

		switch {
		case lv.abort:
			search.Abort()
			incomplete = lv.BreakReason
		default:
			search.Record(lv.OK)
		}
		if lv.OK && st.Phase != capacity.PhaseConfirm && (below == nil || st.Value > below.N) {
			below = &capacity.Point{N: st.Value, X: lv.AchievedRPS}
		}

		// The next level starts after the cooldown, on a fresh bucket.
		next := time.Duration(last+1)*bw + cooldown
		offset = (next + bw - 1) / bw * bw
		if arrivalCtx.Err() != nil && !search.Aborted() {
			incomplete = "the run was stopped before the search finished"
			break
		}
	}

	finishCapacity(out.result, search)
	if incomplete != "" {
		out.warnings = append(out.warnings, result.Finding{
			Code: CodeCapacityIncomplete, Severity: result.SeverityWarn,
			Message: "the capacity search did not finish: " + incomplete,
			Fix:     "the boundary reported is what was found so far; see the levels, and re-run with a longer run.duration or a narrower start..max if the budget ran out",
		})
	}
	return out, nil
}

// levelExecutor installs one level's executor on a runner: the knob's runner holds the
// level's value, every other runner holds its own configured peak, so the probes
// measure each tier at every level.
func (e *Engine) levelExecutor(br *boundRunner, cp *config.Capacity, value float64, step, origin time.Duration) error {
	ex := e.opts.Config.Executor(br.name)
	target := config.PeakRate(ex)
	if br.name == cp.Runner {
		target = value
	}
	if target <= 0 {
		br.setExec(nil)
		return nil
	}
	profile, err := schedule.NewProfile(target, []schedule.Stage{{Duration: step, Target: target}})
	if err != nil {
		return err
	}
	exec, err := e.newExecutor(br, ex, profile, origin)
	if err != nil {
		return err
	}
	br.setExec(exec)
	return nil
}

// judgedLevel is a level's record with the search's own flag.
type judgedLevel struct {
	result.CapacityLevel
	abort bool
}

// judgeLevel reads each runner's measured window and decides whether the level held.
func (e *Engine) judgeLevel(level int, st capacity.Step, cp *config.Capacity, windows map[string]int, budgets map[string]capacity.Budget, below *capacity.Point) judgedLevel {
	lv := judgedLevel{CapacityLevel: result.CapacityLevel{Level: level, Phase: st.Phase, Value: st.Value}}
	in := capacity.Level{Knob: cp.Knob, Value: st.Value, Runner: cp.Runner, Budgets: budgets, Below: below}
	for _, br := range e.runners {
		w := br.collector.Window(windows[br.name])
		in.Measures = append(in.Measures, capacity.Measure{
			Runner: br.name, N: w.N, Achieved: w.AchievedRPS,
			P95MS: w.Response.P95, P99MS: w.Response.P99, ErrorRatio: w.ErrorRatio,
		})
		if br.name != cp.Runner {
			continue
		}
		secs := float64(0)
		if w.N > 0 && w.AchievedRPS > 0 {
			secs = float64(w.N) / w.AchievedRPS
		}
		lv.AchievedRPS = round3(w.AchievedRPS)
		if secs > 0 {
			lv.OfferedRPS = round3(float64(w.Offered) / secs)
		}
		// Little's Law: mean in flight is throughput times mean time in the system.
		lv.MeanConcurrency = round3(w.AchievedRPS * w.Response.Mean / 1000)
		lv.P50MS, lv.P95MS, lv.P99MS = round3(w.Response.P50), round3(w.Response.P95), round3(w.Response.P99)
		lv.ErrorRatio = round3(w.ErrorRatio)
	}
	v := capacity.Judge(in)
	lv.OK, lv.BreakReason, lv.abort = v.OK, v.Reason, v.Abort
	return lv
}

// finishCapacity fills in the boundary, the knee and the USL fit.
func finishCapacity(c *result.Capacity, search *capacity.Search) {
	b := search.Boundary()
	if b.LastOK > 0 || b.FirstBroken > 0 {
		c.Boundary = &result.Boundary{LastOK: b.LastOK, FirstBroken: b.FirstBroken, Stable: b.Stable, Range: b.Range}
	}
	tried := make([]capacity.Tried, 0, len(c.Levels))
	var pts []capacity.Point
	for _, l := range c.Levels {
		tried = append(tried, capacity.Tried{Value: l.Value, Phase: l.Phase, P99MS: l.P99MS})
		if l.OK && l.Phase != capacity.PhaseConfirm {
			pts = append(pts, capacity.Point{N: l.MeanConcurrency, X: l.AchievedRPS})
		}
	}
	c.Knee = capacity.Knee(tried)
	fit := capacity.FitUSL(pts)
	c.USL = &result.USL{
		Fitted: fit.Fitted, Reason: fit.Reason, Lambda: sig(fit.Lambda), Sigma: sig(fit.Sigma), Kappa: sig(fit.Kappa),
		R2: sig(fit.R2), PeakN: sig(fit.PeakN), PeakThroughput: sig(fit.PeakThroughput),
	}
}

// sloBudgets turns the configured SLOs into the units a level is measured in.
func sloBudgets(slo config.SLO) map[string]capacity.Budget {
	out := map[string]capacity.Budget{}
	for _, name := range []string{"http", "db", "redis"} {
		t := slo.For(name)
		if t.IsZero() {
			continue
		}
		var b capacity.Budget
		if t.P95 != nil {
			v := ms(t.P95.D())
			b.P95MS = &v
		}
		if t.P99 != nil {
			v := ms(t.P99.D())
			b.P99MS = &v
		}
		b.ErrorRate = t.ErrorRate
		out[name] = b
	}
	return out
}

func (e *Engine) emitLevel(typ string, level int, knob string, st capacity.Step, lv *result.CapacityLevel) {
	if e.opts.Events == nil {
		return
	}
	data := map[string]any{"level": level, "knob": knob, "value": st.Value, "phase": st.Phase}
	if lv != nil {
		data["ok"] = lv.OK
		data["achieved_rps"] = lv.AchievedRPS
		data["p99_ms"] = lv.P99MS
	}
	e.opts.Events.Emit(typ, data)
}

// sig keeps four significant figures: the fit's coefficients span orders of magnitude.
func sig(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	scale := math.Pow(10, 3-math.Floor(math.Log10(math.Abs(v))))
	return math.Round(v*scale) / scale
}
