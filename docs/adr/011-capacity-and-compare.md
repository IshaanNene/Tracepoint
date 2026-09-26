# ADR-011: How a capacity search runs, and what compare gates

- Status: Accepted
- Date: 2026-09-26
- Phase: 7
- Spec: §5.6 (capacity search), §5.7 (compare)

## Context

§5.6 and §5.7 fix the algorithms - doubling and refinement, the break rules, the USL
form, the order-statistic intervals, the three gates - but leave open how a search
fits the engine, the policy and the result, and several judgements inside the rules.
Each choice below changes what a number in `result.json` or a comparison means.

## Decision

### A search is one run on one timeline

Every level runs on the same runners, so connections stay warm (§5.6), and on the
run's single monotonic clock, so the whole search is one timeline in one
`result.json` and every existing renderer, the incident analysis and the telemetry
samplers work on it unchanged. Executors gained an `Origin`: a level's profile begins
partway into the run. A level starts on a bucket boundary, and the next starts on a
later bucket after the cooldown, so two levels never share a bucket.

A level's figures come from a **measurement window** in the collector: the whole
buckets after `settle`, whose sketches are merged into one as they seal. Percentiles
are never averaged (CLAUDE.md). Once a level has drained, nothing in flight can belong
to its buckets, so they are sealed at once rather than after the seal delay; the
search reads the window and decides the next level without waiting.

Levels record `start_i` and `end_i` (result schema 1.2) so that anything - the
per-level culprit, a chart - can find a level on the timeline.

### The knob's runner holds the level; the probes hold their own rate

Every other runner runs at its own configured peak at every level. The probes then
measure each tier at every level, which is what lets a broken level be attributed.

### The culprit of a broken level is the ordinary analysis, truncated

§5.6 says "culprit per broken level via §5.5". The analysis runs over the timeline up
to the level's end - lower levels are its baseline, nothing later reaches back - and
the incident overlapping the level's window most names the tier. No second, level-only
attribution method exists to drift from the first.

### `run.duration` is the search's budget

A search's length depends on the target, so the policy's `max_duration` cannot be
checked against a schedule. `run.duration` becomes a time budget: a level that would
not fit ends the search with `CAPACITY_INCOMPLETE`. Left unset, it defaults to the
longest the plan can take (`MaxLevels × (step + cooldown)`); if the policy's ceiling
is lower, the budget is shortened to it with `CAPACITY_BUDGET_CLAMPED` rather than the
run refused - the budget only bounds the search. A `run.duration` someone wrote is
held to the ceiling like any other. `max` is held to the policy's rate ceiling (or its
in-flight ceiling, for `concurrency`).

### A search is not judged against its SLOs as a whole

It breaks them on purpose. Each level is judged against every runner's SLOs instead,
and the run exits 0 unless it failed. Validity is still judged over the whole run.

### The closed-model plateau

"Achieved < 90% of offered" has no meaning when offered load is an output. For
`concurrency`, a level plateaus when its extra users bought less than a tenth of the
throughput proportional scaling from the highest lower level that held would give.

### The knee

The first level, by value, whose p99 exceeds twice the lowest p99 at a lower level -
the same factor the strain finder uses.

### compare gates runners, not labels

Deltas and intervals are reported per label; gates apply per runner. A label holds a
fraction of its runner's samples, so its p99 interval is wider and its gate would
fail as insufficient far more often, turning noise into red CI.

### A rise too large to clear on too little data fails

When p99 rose past the relative gate but either interval cannot be formed, the gate
fails as `COMPARE_INSUFFICIENT_DATA` (exit 1, as `docs/ERRORS.md` registered in phase
0): a CI gate that passes whatever it cannot judge is not a gate. A rise under the gate
passes on any amount of data.

### The comparison is its own contract

`compare --output json` and `compare_runs` return one document with its own schema,
`compare.schema.json` (1.0), validated in tests like every other emitted document.

## Consequences

- A search's result reads like any run's, plus `capacity`; nothing that renders or
  analyses a run needed a special case except the whole-run SLO checks.
- Sealing early made sealing concurrent with the sweeper for the first time, which
  exposed an unsynchronised sketch read in `Snapshot`; it is now locked.
- A generator that falls behind at one level makes the whole search invalid, because
  validity is judged per run. Per-level validity is left for later.
- The intervals cover sampling noise, not a changed environment. `COMPARE_TAIL_ONLY`
  notes when only p99 moved, the signature a paused host shares with a real tail
  regression.

## Alternatives considered

- **A run per level.** Simpler engine, but cold connections at every level, one
  result per level, and no single timeline for attribution.
- **Refuse any search the policy's duration might not fit.** Safe, but it refuses most
  searches under the server default of 10 minutes, for a budget the search never has
  to use.
- **Gate labels too.** Rejected above.
