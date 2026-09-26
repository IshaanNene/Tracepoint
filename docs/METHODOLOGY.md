# Methodology

How TracePoint turns three latency series and some server telemetry into a verdict,
and exactly how far that verdict can be trusted. Every constant below lives in one
place — `analysis.DefaultParams()` — and every rule is a pure function of
`result.json`, so a verdict can be audited against the document that produced it.
The reasoning behind the choices is in [ADR-005](adr/005-correlation-methodology.md);
this file is the reference.


## What the tool can claim

TracePoint observes **association on a shared clock**. It has no distributed trace
and no link from a particular request to a particular query. When the application's
latency and a storage probe's latency move together, and the datastore's own
telemetry agrees, that is evidence — correlational evidence. Every verdict carries
the standing caveat *"Shared-clock correlation shows association, not causation."*,
and the tool will answer `inconclusive` or `unobserved` rather than guess.

## Three latencies

Every operation records when it was due, dispatched, picked up by a worker, holding a
connection, and finished. Three durations follow:

| Name | Measured | Used for |
| --- | --- | --- |
| `response_time` | end − intended send time | SLOs — what a user feels, corrected for coordinated omission |
| `service_time` | end − connection acquired | Tier attribution — what the target did |
| `client_wait` | connection acquired − intended | The generator's own delay: dispatch lag, queue wait, pool wait |

Hot buckets are judged on **service time**, so time spent queueing inside the
generator is never charged to a database. SLOs are judged on **response time**,
because that is what was promised to users.

## Open and closed models

Two executors answer two different questions ([ADR-003](adr/003-executors-and-desugaring.md)).

**`arrival-rate` (open).** Arrivals follow a schedule whatever the target does, so
offered load is an input. Each iteration's intended send time comes from the
schedule, which is what makes `response_time` coordinated-omission correct.

**`vus` (closed).** A population of virtual users, each running one iteration after
another as fast as the target answers. The stages set the number of users: every
100ms the executor reads the profile, rounds it to a whole number of users, and
starts or retires users to match. A user being retired finishes its current
iteration first. Offered load is an output: when the target slows, each user waits,
and the population offers less - exactly as a fixed pool of real clients would.

There is no schedule in a closed model, so there is nothing to have been late
against: an iteration's intended time is the moment its user starts it. Under `vus`,
`response_time` and `service_time` therefore differ only by the pool wait inside the
generator, and neither can be coordinated-omission corrected. That is a property of
the model, not a defect. SLOs judged on a closed run say "users of a pool this size
saw this"; they do not say what an open stream of the same average rate would see.

When the profile ends, users stop starting iterations, those mid-iteration have
`run.grace` to finish, and anything still running is recorded as `canceled`.

## Journeys

`http.requests` is sugar for single-step journeys, so both run through one engine.
Timing and failure within a journey:

| Rule | Why |
| --- | --- |
| The first step is timed from the iteration's intended send time; each later step from the moment it actually starts | A late journey is charged once, to its first step, instead of compounding its lateness into every step after it |
| Think time is a pause between steps, never after the last one, and is in no latency | Think time models a person reading; it is not the target's time |
| A step that fails - an error, an unexpected status, a failed `expect` - ends the iteration | The steps after it would act on a response that did not arrive, and their failures would be noise |
| Extraction reads only successful responses; a value that is missing fails the step as `extract_failed` | A literal `{{token}}` is never sent |
| A `unique` feeder that runs out fails the step as `extract_failed`, logged once | The configuration asked for no reuse; repeating a row silently would break that promise |
| Cookies are kept per virtual user for the whole run under `vus`; under `arrival-rate`, per iteration, and only for journeys of more than one step | A closed-model user is a session; an open-model arrival is a new visitor |

Every step is its own label, `journey/step`, with its own sketches and thresholds; a
request's label is its name. There is no journey-level latency series.

## Buckets and evidence

Every runner records into the same time buckets (`run.bucket`, default 1s) on one
monotonic clock. A bucket with fewer than `run.min_samples` completed operations
(default 20) is **insufficient**: it is drawn in charts but never used as evidence —
never hot, never part of a baseline, never in a correlation. Warm-up buckets are
likewise excluded from all of it.

When the generator is starved, few operations complete per bucket and every bucket
can be insufficient. The remedy is wider buckets or a higher probe rate, never a
lower bar: a p99 of five observations is the largest of five numbers.

## Hot buckets

A bucket is hot when either rule fires; the result records which (`absolute`,
`relative` or `both`).

**Absolute**: p99 service time above the runner's threshold — its `slo.<runner>.p99`
if set, else `--<runner>-threshold`, else 100ms. `analysis.thresholds` records the
value and its source.

**Relative**: against the median of the previous 30 *steady* buckets — eligible
buckets that were not themselves hot, so an ongoing slowdown never becomes its own
baseline — the bucket must clear all three bars:

| Bar | Value | Why |
| --- | --- | --- |
| Modified z-score `0.6745·(x − median)/MAD` | > 3.5 | Median and MAD are robust to the very outliers being hunted |
| Ratio to the median | ≥ 3× | A z-score alone fires on noise in a very stable series |
| Distance above the median | ≥ 5ms | A 1ms service wobbling to 3ms is not a finding |
| Baseline size | ≥ 10 steady buckets | Too early to judge otherwise |

The MAD is floored at 1% of the median — the sketches' relative accuracy, below
which differences are measurement noise — so a perfectly flat baseline cannot turn a
flicker into an infinite z-score.

### Generator stalls

A bucket is a **generator stall** when all of these hold:

| Condition | Value | Why |
| --- | --- | --- |
| Every runner with eligible data there saw its client-wait p99 jump | ≥ 3× its own run median and ≥ 5ms above it | A pause delays every runner's sends at once; a runner short of workers delays only its own |
| At least this many runners have data there | 2 | With one runner a pause and a queue look the same |
| No tier's service time is over its threshold by more than the pause explains | a service-time rise over 10× the bucket's largest client-wait rise is the target moving | A pause that lands mid-request adds about its own length to service time; a real fault adds far more. On the known-answer host a Postgres lock raised db service time ~200× more than it raised client wait |
| The run of such buckets is short | ≤ 2 consecutive buckets | A pause lasts milliseconds and can straddle one boundary; a real storage stall also slows the generator — blocked workers pile up — but it lasts |

That is the generator's whole process being paused — by a virtualised host taking
the CPU back, for example.

A stalled bucket is neither hot nor part of any baseline, exactly like an ineligible
one, and the run carries a `GENERATOR_STALL` warning listing the buckets. With a
single runner there is nothing to compare against, so no bucket is ever called a
stall. (Added after phase 5, when a microVM's steal time produced one-bucket pauses
that the relative rule faithfully flagged as incidents on clean runs.)

## Incidents

1. Each runner's hot buckets are merged into **episodes**, bridging gaps of at most
   one bucket.
2. Episodes of different runners join one **incident** when, shifted by at most one
   bucket, they would overlap — so adjacent episodes join and episodes with a bucket
   between them do not.
3. Each runner is judged over the incident window: hot or not, its peak service p99,
   its severity (peak ÷ threshold), and whether it was **sufficient** there — at least
   half the window's buckets eligible.

Classes, checked in this order:

| Class | Condition | Meaning |
| --- | --- | --- |
| `client_limited` | every hot runner's client wait was ≥ 50% of its response p99 (median over its hot buckets) | The generator set the pace; the target is not implicated |
| `correlated` | the application and at least one storage runner hot | That storage tier is a candidate cause |
| `storage_only` | storage hot, application not | A bottleneck that has not reached users yet |
| `unobserved` | application hot, no storage runner hot, and some probe insufficient or none configured | The tool declines to attribute |
| `app_only` | application hot, every storage probe healthy **on sufficient data** | The application tier itself |

`app_only` is an accusation, so it requires probes that were actually watching.

### Culprit ranking

Inside a correlated incident each hot storage runner is scored:

```
score = 2 · clamp(lead, −3, +3) + clamp(log2(severity), 0, 4) + min(corroborating signals, 2)
```

`lead` is how many buckets the runner went hot before the application. Lead carries
the most weight because cause precedes effect; severity is logarithmic so a tier ten
times over its threshold does not drown out one that moved first; corroboration is the
only signal that is not latency. Scores within 0.25 of the best are a **tie**, and a
tie is reported: the incident lists `tied_with`, and the digest's culprit is `null`.

A **negative** best score names no culprit. It can only arise when every hot storage
tier went hot after the application, uncorroborated and not severe enough to make
up for it — the evidence points away from them. The incident stays `correlated`
with `culprit: null`, and the verdict is `inconclusive`, saying the storage tier
followed the application.

### Incident confidence

| Class | High | Medium | Low |
| --- | --- | --- | --- |
| correlated | untied culprit with corroborating telemetry | untied culprit | a tie, or no culprit |
| storage_only | corroborated | otherwise | — |
| app_only, client_limited | — | always | — |
| unobserved | — | — | always |

## Whole-run correlation

Spearman's rho between the application's service p99 series and each storage
probe's, over buckets eligible in both, at lags −3…+3. At lag *L* application bucket
*i* is paired with storage bucket *i − L*: a positive lag means the storage tier moved
first. The best lag maximises |rho|, preferring the smaller |lag| and then the
positive side on a tie. The lean is `none` below 10 pairs or rho 0.3, `app` when the
application led, `storage` otherwise.

A high rho at lag 0 over a whole run is weaker evidence than it looks: both series
respond to the same load. It counts towards confidence, but it is **never promoted to
an answer on its own**.

## Telemetry

Samplers read on the shared offset clock, each on its own goroutine and its own
read-only connection, so a datastore that freezes freezes only its own sampler. Every
sample carries `t_ms`; datastore samples also carry `sample_ms`, how long the reading
took — a server that stalls stalls its sampler too, and that is itself a signal.
Counters are reported as deltas between readings.

| Sampler | Keys |
| --- | --- |
| `generator` (always on) | `cpu_ratio`, `goroutines`, `heap_bytes`, `gc_pause_p99_ms`, `sched_latency_p99_ms`, `dispatch_lag_p99_ms` and `in_flight` per runner |
| `postgres` | `waiting_by_type` (client backends by `wait_event_type`), `sessions_active`, `sessions_idle_in_tx`, `sessions_waiting_lock`, `sessions_tracepoint`, `sessions_other`, `locks_waiting`; deltas `commits`, `rollbacks`, `deadlocks`, `temp_bytes`; `cache_hit_ratio` over the interval |
| `mysql` | `threads_running`, `threads_connected`; deltas `row_lock_waits`, `row_lock_time_ms`; `buffer_pool_hit_ratio`, `queries_per_sec` |
| `redis` | `ops_per_sec`, `connected_clients`, `blocked_clients`, `used_memory_bytes`, `hit_ratio`; deltas `evicted_keys`, `rejected_connections`; `latency_max_ms` with `telemetry.redis.latency` |

A sampler that cannot connect, lacks a grant, or never manages a reading is reported
`available: false` with a reason, and the run is marked degraded with
`TELEMETRY_UNAVAILABLE`. Postgres without `pg_read_all_stats` still samples, with its
limitation stated in `reason`. In Redis cluster mode INFO reaches one node.

### Corroborating signals

Over the incident window, padded by a bucket either side, a signal fires when its
peak (or trough) departs from its median outside the window by both an absolute and
a relative margin:

| Source | Signal | Fires on |
| --- | --- | --- |
| postgres | `locks_waiting`, `sessions_waiting_lock` | rise ≥ 1 and ≥ 2× |
| postgres | `sessions_active` | rise ≥ 3 and ≥ 2× |
| postgres | `deadlocks` | any |
| postgres | `cache_hit_ratio` | fall ≥ 0.05 from ≥ 0.5 |
| postgres | `temp_bytes` | rise ≥ 1 MiB and ≥ 2× |
| mysql | `row_lock_waits` | rise ≥ 1 and ≥ 2× |
| mysql | `threads_running` | rise ≥ 3 and ≥ 2× |
| mysql | `buffer_pool_hit_ratio` | fall ≥ 0.05 from ≥ 0.5 |
| redis | `blocked_clients` | rise ≥ 1 and ≥ 2× |
| redis | `latency_max_ms` | rise ≥ 50ms and ≥ 3× |
| redis | `evicted_keys`, `rejected_connections` | any |
| redis | `hit_ratio` | fall ≥ 0.1 from ≥ 0.3 |
| any datastore | `sample_ms` | rise ≥ 50ms and ≥ 3× |

**Throughput is deliberately not a signal.** It falls when a server stalls, but it
falls just as far when the application stops calling that server because a different
tier is stalled — a database lock starves the cache of requests. A fall in throughput
cannot tell cause from victim, so it corroborates nothing.

## Verdict

The verdict is assembled from fixed sentence templates filled with measured numbers:
the same result always yields the same words.

- An **invalid** run's verdict is `client`, with the validity findings as evidence.
- Otherwise incidents that reached users are grouped by the answer they support — a
  storage culprit, `app`, `client`, `unobserved` or a tie — and the answer covering the
  most buckets wins. Two storage tiers each named alone by an equal share is a tie at
  the level of the run: `inconclusive`.
- With no user-facing incident, `storage_only` incidents yield a **masked** verdict
  naming the storage tier; it is a warning about the future and never high confidence.
- A missed budget with no incident is `inconclusive`: the run was slow evenly and there
  is no moment at which one tier moved and another did not.
- Otherwise `none`.

**Confidence** is a rubric of three points: sufficient samples (≥ 90% of the involved
runners' measured buckets), consistency (≥ 2 supporting incidents, or a storage-leaning
whole-run |rho| ≥ 0.7), and corroboration (datastore telemetry for a storage verdict;
quiet datastore telemetry through every incident for `app`; a generator finding or
dropped arrivals for `client`). Three points is high, two medium, fewer low.
`inconclusive` answers are always low, and a **degraded** run is capped at medium.

## Validity

Read before anything else.

| Finding | Severity | Rule |
| --- | --- | --- |
| `GENERATOR_BEHIND` | error | dispatch lag p99 > max(5ms, 5% of a bucket) |
| `CLIENT_CAPPED` | error | dropped arrivals > 1% while service time stayed flat |
| `TARGET_SATURATED` | warn | dropped arrivals > 1% while service time rose |
| `SAME_HOST_TARGET` | warn | a target resolved to loopback |
| `RATE_LIMITED` | warn | > 5% of responses were 429 |
| `INSECURE_TLS` | warn | certificate verification disabled |
| `TELEMETRY_UNAVAILABLE` | warn | a configured sampler delivered nothing |
| `GENERATOR_STALL` | warn | in some bucket every runner's client wait jumped together (≥ 3× its median and ≥ 5ms above it) |
| `LITTLES_LAW_INCONSISTENT` | warn | see below |
| `RUN_ABORTED` | warn | the abort guard stopped the run |

**Saturated or capped.** Service time "rose" when the median p99 of the buckets that
dropped arrivals is at least 1.5× that of the steady buckets that did not. When every
bucket dropped, the first few are the only baseline. A flat line is the cautious
reading: the ceiling was ours.

**Little's Law.** Mean concurrency equals throughput × mean time in the system, and
the peak concurrency in a bucket can never be below the mean. A runner whose median
bucket has `rps × mean service time` above 1.2 × its peak in-flight count is physically
inconsistent: something waited where it was not measured.

**The same-host caveat.** A loopback target shares the generator's CPU. Results are
indicative; a capacity number needs the generator on another machine.

## Recommendations

Fixed rules with stable ids, in priority order: fix the measurement, gather missing
evidence, follow the verdict. An `action: rerun` recommendation carries the exact
`--set` overrides that apply it, and is offered only where an override can apply
cleanly — a staged profile has no single rate to override, so rate advice there is
`configure`.

| Id | When | Action |
| --- | --- | --- |
| `raise-max-in-flight-<runner>` | `CLIENT_CAPPED`, `GENERATOR_BEHIND` with every worker busy, or a `client_limited` incident | rerun with `max_in_flight` = offered rate × p99 service time × 1.2, at least double the current |
| `lower-rate-<runner>` | `GENERATOR_BEHIND` with workers to spare | rerun at half the rate |
| `measure-below-saturation-<runner>` | `TARGET_SATURATED` | rerun at 0.7× the rate |
| `grant-telemetry-<sampler>` | `TELEMETRY_UNAVAILABLE` | configure, with the grant |
| `add-storage-probes` | unobserved incidents and no storage runner | configure |
| `raise-probe-rate-<runner>` | a probe insufficient during unobserved incidents | rerun at 1.5 × min_samples per bucket |
| `enable-telemetry-<sampler>` | a storage verdict with that tier's sampler off | rerun with `telemetry.<sampler>.enabled=true` |
| `investigate-db`, `investigate-redis`, `profile-app` | that verdict | investigate the incident windows |
| `configure-slo` | verdict none with no budgets | configure |
| `raise-load` | verdict none | rerun at twice the rate |
| `separate-generator-host`, `dedicated-generator-host`, `exempt-rate-limiter` | `SAME_HOST_TARGET`, `GENERATOR_STALL`, `RATE_LIMITED` | configure |

## Strain

On a run whose application load ramps - its stages change level - the strain finder
reports where the application starts to struggle. It reads the application runner's
eligible buckets (not warm-up, not insufficient):

1. **Early baseline**: the median p99 service time of the first 10% of eligible
   buckets, at least 5 and at most 30.
2. **Strain**: the first 3 consecutive eligible buckets after the baseline whose p99
   service time is more than 2× that baseline.
3. **Load at strain**: the first strained bucket's measured peak in-flight count
   ("users") and its throughput ("req/s"). These are what was measured, not what the
   stages asked for, so a generator that fell behind cannot overstate them.

Found, it reports *"strain begins at ~N users (~R req/s)"*, and recommends the next
capacity window around that point: from half the level to one and a half times it,
in requests per second for an open run or users for a closed one. Not found, it
reports *"no strain up to ~N users"*, and the next window runs from the highest level
reached to twice it. Too few eligible buckets for a baseline and a run of three, and
the finder says nothing.

| Constant | Value |
| --- | --- |
| Early share, minimum, maximum | 10%, 5, 30 buckets |
| Strain factor | 2× the early baseline |
| Sustained for | 3 consecutive buckets |

Service time rather than response time is judged, as for hot buckets, so a generator
that queues cannot manufacture strain.

## Capacity search

A `capacity` section turns a run into a search for the load at which the system stops
meeting its SLOs. The constants are in `internal/capacity`.

**Levels.** The search starts at `start` and doubles until a level breaks or `max` is
reached. It then refines between the last level that held and the first that broke -
by bisection, or with `refine: linear:N` by passes of N evenly spaced levels - until
the gap is within the resolution (default: 1, or 5% of the first broken level,
whichever is larger). Levels are whole numbers. With `confirm` (the default) both
sides are run once more; if either flips, the boundary is reported as a range - from
the highest level that held on every run to the lowest that broke on every run -
rather than a point, because a boundary that moves between runs is not a number
worth quoting.

**One timeline.** Every level runs on the same runners, so connections stay warm, and
on the run's single clock. A level starts on a bucket boundary, holds its value for
`step_duration`, and its first `settle` (default 20% of the step) is discarded; its
measured window is the whole buckets after that. The window's figures come from
sketches merged across its buckets - never from averaging per-bucket percentiles.
Nothing else is in flight once a level has drained, so its buckets are sealed at
once, and the next level starts after `cooldown` on a later bucket: two levels never
share one. The knob's runner holds the level's value; every other runner holds its
own configured peak at every level, so the probes measure each tier throughout.

**Breaking.** A level breaks when

| Rule | Why |
| --- | --- |
| Any runner breaches any of its SLOs in the level's window (response time, and error rate) | Capacity is where the promise stops being kept, whichever tier breaks it |
| Open model: the knob's runner completes less than 90% of the rate offered | A plateau: the target cannot keep up, whatever its latency says |
| Closed model: the extra users bought less than a tenth of the throughput proportional scaling from the highest lower level that held would give | Offered load is an output of a closed model, so a plateau is more users buying almost nothing |
| More than half the knob runner's operations failed | Not a level that broke but a target that is failing: the search stops at once and says so |

A search is not judged against its SLOs as a whole - it breaks them on purpose - so
it exits 0 unless the run itself failed. `run.duration` is its time budget: a level
that would not fit ends the search with `CAPACITY_INCOMPLETE`. Left unset, the budget
is the longest the plan could take; a policy ceiling below that shortens it, with
`CAPACITY_BUDGET_CLAMPED`, rather than refusing the run. `max` is held to the policy's
rate (or in-flight) ceiling like any configured load.

**The culprit of a broken level** is found by the ordinary analysis, run over the
timeline up to the level's end, so the lower levels are its baseline and nothing later
can reach back into it. The incident that overlaps the level's window most names the
tier: its culprit when correlated, the hot storage tier when storage-only, the
application when app-only. A level that broke on a plateau with nothing going hot, or
on the generator's side, names none.

**The knee** is the first level, by value, whose p99 exceeds twice the lowest p99 of
any lower level: where latency began climbing sharply.

**Mean concurrency** of a level is Little's Law over its window: throughput times
mean response time. It is the x of the scalability fit.

### The Universal Scalability Law

Over the levels that held (confirmation runs aside), TracePoint fits

X(N) = λN / (1 + σ(N−1) + κN(N−1))

where N is the measured mean concurrency and X the throughput: λ is throughput per
unit of concurrency without interference, σ contention (the serialised share of the
work) and κ crosstalk (coherency cost, which makes throughput fall after a peak). For
a fixed λ the model is linear in σ and κ once rearranged, so the fit is a
two-variable non-negative least squares - σ, κ ≥ 0 - inside a one-dimensional search
over λ that minimises the squared error in throughput itself. The predicted peak is at
N* = √((1−σ)/κ); with κ = 0 there is none, and none is invented.

The fit is reported only with at least 4 levels and R² ≥ 0.9, because an
extrapolation from too little, or one that explains too little, is worse than none.
It is a model, not a measurement: a target with a hard ceiling - a fixed pool, a
semaphore - is not the smooth contention USL describes, and its predicted peak can
overshoot the real one. The boundary, which was measured, is the number to quote.

## Compare

`tracepoint compare` (and `compare_runs`) judges a run against a baseline. The
constants are in `internal/compare`.

**Is a change real?** Each run's result carries its whole-run response-time sketch. For
p99 the distribution-free confidence interval is the pair of order statistics at ranks
n·q ± 1.96·√(n·q·(1−q)), read from the sketch - a 95% interval that assumes nothing
about the shape of the distribution. When a rank falls outside [1, n] there is too
little data for an interval, and the change is `insufficient`. A change is called
`slower` or `faster` only when the two runs' intervals do not overlap. That is
conservative by design: two 95% intervals from the same distribution fail to overlap
far less often than 5% of the time - in 300 simulated A/A pairs of 3,000 samples,
never - so noise is not reported as a regression, at the cost of missing some small
real ones.

**Gates.** Any failure makes the comparison a regression and the command exit 1:

| Gate | Fails when |
| --- | --- |
| `budget_crossing` | The current p99 is over its budget and the baseline's was not. Budgets are the runs' own SLOs unless `--budget` names them. No significance is needed: the promise was broken |
| `relative_increase` | p99 rose by more than 10% **and** more than 5ms, and the change is `slower`. When the rise is that large but the change is `insufficient`, the gate fails as `COMPARE_INSUFFICIENT_DATA`: it can be neither confirmed nor cleared |
| `error_rate_increase` | The error rate rose by more than 0.5 percentage points |

Gates are judged per runner; labels are compared and reported, not gated, because a
label's samples are a fraction of its runner's.

**What the intervals do not cover.** They are built for sampling noise. A run whose
environment changed - a paused virtual machine, a noisy neighbour, a server sharing a
process with something that stalls - has a genuinely different tail, and the intervals
rightly call it a change. A pause holding requests for P shares out over a run of
length D as P/D of its requests, so a p99 gate needs runs much longer than a hundred
times the longest pause, and a quiet host. When only the tail moved - p99 over the
gate, p50 and p95 within it - the comparison notes it with `COMPARE_TAIL_ONLY`; the
gate still fails, because a real regression can live in the tail alone.

**Incidents** are paired per runner: a baseline and a current incident match when
their windows meet within two buckets. A matched pair is `worsened` or `improved` when
the runner's peak p99 moved by more than the relative gate's thresholds, else
`unchanged`; the rest are `new` or `fixed`.

**Warnings.** Comparing an invalid run is flagged as an error-severity warning: its
numbers describe the generator. Differing configurations are listed key by key, and a
runner present in only one run is named rather than dropped.

## The known-answer suite

`make e2e` runs TracePoint against `tools/faultbox` — an application over real
Postgres and Redis containers — injecting each fault at a known offset from the run's
first request, and requires the matching answer within one bucket of when the fault
actually held (faultbox records that, so the suite asserts against what happened):

| Fault | Required |
| --- | --- |
| none | zero incidents, verdict `none`, exit 0 |
| `LOCK TABLE items` (endpoint and probe both read it), 5s | `correlated`, culprit db, postgres telemetry, exit 1 |
| `LOCK TABLE probe_only` (only the probe reads it), 5s | `storage_only`, exit 0 |
| 400ms handler delay, 5s | `app_only`, verdict app, exit 1 |
| Redis `DEBUG SLEEP 3` | `correlated`, culprit redis, redis telemetry, exit 1 |
| 300ms endpoint, `max_in_flight: 2` | `client_limited`, invalid, verdict client, exit 4; no incident blames the target |

Nightly CI runs it five times to catch flakes.
