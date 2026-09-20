# ADR-002: Relative-error sketches and the memory budget

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §2 (never average percentiles), §3.4 (metrics and memory model)

## Context

Two requirements pull against each other. Percentiles must be accurate and mergeable:
we need a p99 per bucket, a p99 per label, a p99 for the whole run, and — for
`compare` — enough distributional information to say whether a change is real. And
memory must not grow with run length: a ten-minute soak at 2k req/s is 1.2M
operations, and keeping them is 10–20 MB of live heap that never shrinks, plus the
GC pressure of holding it.

Averaging per-bucket percentiles would solve the memory problem and produce a number
that means nothing: the mean of per-second p99s is not the p99 of the run, and the
error is unbounded and in an unpredictable direction. §2 forbids it, correctly.

## Decision

**Use a relative-error sketch: DataDog's DDSketch, at 1% relative accuracy.**

DDSketch maps a value to a bucket index `ceil(log_gamma(x))` with
`gamma = (1+alpha)/(1-alpha)`. Any quantile read back is within `alpha` relative error
of the true quantile — a *relative* guarantee, which is the right shape for latency,
where being 1% wrong about a 2-second p99 matters and being 1% wrong about a 200
microsecond p50 does not. Merging is bucket-wise addition of counts, so it is exact,
associative and commutative: merge order cannot change the answer. That property is
what makes per-worker shards, per-bucket sealing and cross-run comparison all sound,
and it is asserted directly in a property test.

**The memory budget.** With `alpha = 0.01`, `gamma = 1.01/0.99` and `ln(gamma) =
0.0200`. Covering a latency range from 1 microsecond to 100 seconds — eight orders of
magnitude, far wider than any real run — needs `ln(1e8)/0.0200 = 921` buckets. A
realistic 1ms–10s spread needs about 460, and the store is sparse, so typical
occupancy is much lower. Call it 1 KB per open sketch as a pessimistic figure.

Only a few buckets are open at once. A bucket seals when no in-flight operation can
still land in it, which is `now > bucket_end + max operation timeout`, so the open
window is `ceil(timeout / bucket_width) + 1` — eleven buckets at the default 10s
timeout and 1s buckets. The live sketch count is therefore bounded by
`runners x labels x open_window`, not by run length: 3 x 50 x 11 = 1650 sketches,
about 1.6 MB worst case. Whole-run sketches add `runners x (labels + 1)` = 153, which
are denser but still roughly 1.2 MB. Both figures are independent of how long the run
lasts.

**Sealed buckets are reduced and freed.** On sealing, a bucket is merged into the
whole-run sketches (unless it falls in warm-up), reduced to a fixed-size summary —
count, per-class error counts, p50/p90/p95/p99/p99.9/max/mean, bytes, dropped,
peak in-flight — and its sketch is released.

**Per-label timelines have a retention budget.** Sealed summaries do accumulate: they
are the timeline, and the timeline is the product. Per *runner* that is cheap — 600
buckets over ten minutes, a few hundred KB — and it is always retained, because every
analysis in §5.5 reads it. Per *label* it is `labels x buckets`, which at 50 labels
and a ten-minute run is 30,000 summaries and roughly 6 MB, enough to break the
flat-heap soak criterion on its own. So per-label timelines are retained only while
`labels x buckets` stays within a budget (default 20,000 entries, configurable);
beyond it they are dropped, a `LABEL_TIMELINE_DROPPED` warning is recorded, and the
per-runner timeline plus the whole-run per-label sketches and summaries carry on
unaffected. **No analysis depends on per-label timelines** — they exist for the HTML
report — so dropping them changes presentation, never a verdict.

**The hot path takes no global lock and allocates nothing.** Recording an outcome
writes to a per-worker shard chosen by worker index, so workers never contend;
shards are merged only when a reader needs them, which is at seal time. Sketch
buckets are pre-sized and reused, so steady-state recording is amortised 0 allocs/op.
Both properties are benchmarked, not assumed.

**Small samples are not silently trusted.** A bucket with fewer than `min_samples`
operations (default 20) is marked insufficient. It is drawn in charts, because a gap
would be misleading, but it can never make a bucket hot, join an incident or support
a verdict. A p99 taken from eleven samples is the maximum of eleven samples wearing a
percentile's name.

## Consequences

- Quantiles anywhere in the system come from merging sketches and reading the merged
  result. There is no code path that averages a percentile.
- `compare` gets what it needs: each run's serialised whole-run sketch plus its
  count, which is exactly the input to a distribution-free order-statistic confidence
  interval (§5.7). This is why sketches, not just summaries, are written into
  `result.json`.
- p99.9 is reported but is meaningfully noisy below a few thousand samples; the
  renderers say so rather than dropping the column.
- The 1% accuracy figure appears in `result.json` next to every sketch, so a reader
  can tell a real 3% change from sketch noise.
- Acceptance is measured, not asserted: the soak run asserts heap growth under 10%
  after warm-up, and the record-path benchmark asserts 0 allocs/op.

## Alternatives considered

- **Keep every observation.** Exact, and unbounded. It is also the only way to get an
  exact p99.9, which is not worth 20 MB and a GC problem. The optional
  `--raw out.ndjson.gz` sink exists for anyone who genuinely needs the raw data, and
  it streams to disk rather than to the heap.
- **HDR histograms.** Mergeable and fast, but they guarantee *absolute* precision
  within a configured range, so you must know the latency range in advance. Pick the
  range too narrow and the tail clips; too wide and memory balloons. Relative accuracy
  needs no such guess.
- **t-digest.** Excellent tail accuracy for the space, but its merge is
  order-dependent and only approximately associative, and the accuracy guarantee is
  empirical rather than proven. Both matter here: shards and buckets are merged in
  whatever order they happen to be read, and `compare` makes statistical claims on
  top of the result.
- **Reservoir sampling.** Bounded memory, but it throws away the tail, which is the
  only part of the distribution this tool cares about.

## Verification performed at implementation time (phase 1)

The memory and allocation claims above were measured against `DataDog/sketches-go
v1.4.8` before the package was built around it, using a logarithmic mapping at 1%
relative accuracy over dense stores.

| Claim | Measured |
| --- | --- |
| The record path allocates nothing in steady state | `Add` into a warmed sketch: **0 B/op, 0 allocs/op**, 26.7 ns/op |
| Merging is cheap enough to do per sealed bucket | `MergeWith` of two 100k-observation sketches: **0 allocs/op**, 301 ns/op |
| A serialised sketch is small enough to embed in `result.json` | 200,000 observations spanning 50µs–10s encode to **971 bytes** — below the 1 KB pessimistic figure used in the budget above |
| Serialisation is lossless | `Encode` / `DecodeDDSketch` round-trips p50, p99 and p99.9 exactly, and preserves the count |

`Encode`/`DecodeDDSketch` is the library's own compact binary format, base64-encoded
into `result.json`. It is not protobuf — but note that `sketches-go` imports its own
protobuf bindings from the same package, so `google.golang.org/protobuf` arrives as an
indirect dependency whether or not those methods are called. Choosing the binary codec
therefore buys a smaller, faster payload rather than one fewer module; ADR-007 records
protobuf explicitly rather than leaving it unexplained in `go.mod`.

### Amendment: how the hot path avoids a global lock

The spec says "per-worker shards merged on read". Sharding the *sketches* by worker
turned out to be the wrong trade: it multiplies per-bucket state by the shard count,
and the memory budget above assumes `runners x labels x open_window` sketches, not
`x shards` as well. At 8 shards, 50 labels and an 11-bucket open window that is 13,200
live sketches holding a handful of samples each — heavyweight structures almost all of
which are nearly empty.

What is actually implemented:

- **Counters** — operation count, per-class error counts, bytes, status codes, offered
  and dropped arrivals — are sharded per worker and updated atomically. No lock at all.
- **Sketches** are held once per `(label, open bucket)` and guarded by that slot's own
  mutex. There is no global lock, and in steady state traffic spreads across one mutex
  per label.

The critical section is a single `Add`, measured above at 27ns. At 2,000 operations a
second that is a duty cycle of roughly 0.005%; contention is not a realistic concern at
any rate this tool can generate, and `BenchmarkRecordParallel` measures it rather than
assuming. The memory figures in the budget above therefore hold as written.
