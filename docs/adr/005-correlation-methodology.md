# ADR-005: How hot buckets, incidents, culprits and verdicts are derived

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §1 (mental model), §5.5 (analysis)

## Context

The tool answers "which tier caused this slowdown" from timing alone. It has no
distributed traces and no causal link between a specific request and a specific
query. What it has is three latency series on one clock, plus server-side telemetry on
that same clock. The method must be honest about what that can and cannot establish,
and it must be deterministic, because a verdict that changes between two analyses of
the same data is worthless.

The mental model, from §1: storage runners are **probes as well as load**. They query
the datastore directly, so their latency is a direct measurement of that tier's health
under the combined pressure of the application's traffic and our own. When application
latency moves together with a probe's latency, and the datastore's own telemetry
agrees, that is evidence. It is correlational evidence, and the tool says so every
time it speaks.

## Decision

### 1. Hot buckets — two rules, both recorded

A bucket is hot when either rule fires, and the result records which:

- **Absolute**: p99 *service* time exceeds the runner's threshold — the `slo` p99 if
  configured, else `--<runner>-threshold`, else 100ms. Service time, not response
  time, because time spent queueing inside our own generator must not be charged to a
  database.
- **Relative**: the bucket stands out against a rolling median of the previous 30
  steady buckets (at least 10 required) on **all three** of:
  - modified z-score `0.6745 * (x - median) / MAD > 3.5`,
  - p99 at least 3x the rolling median,
  - p99 at least 5ms above it.

The modified z-score uses the median and median absolute deviation rather than the
mean and standard deviation, because a latency series contains the very outliers we
are hunting and they would inflate a standard deviation enough to hide themselves. The
constant 0.6745 is the 0.75 quantile of the standard normal, which scales MAD to be
comparable to a standard deviation for normally distributed data; 3.5 is the
conventional threshold. The two extra guards exist because a z-score alone fires on
noise in a very stable series: a service sitting at 1.0ms with almost no variance will
produce huge z-scores at 1.4ms, which is not a finding. Requiring 3x *and* 5ms
absolute keeps the rule from crying wolf on healthy systems.

Buckets below `min_samples` are never hot, in either rule.

### 2. Incidents, not spike rows

Each runner's hot buckets are merged across gaps of one bucket into episodes, then
episodes are overlapped across runners with a tolerance of one bucket either side. The
tolerance absorbs sampling boundaries: a spike straddling a bucket edge should not
read as two unrelated events in two tiers.

An incident is classified by which tiers were hot and which were demonstrably healthy:

| Class | Condition | What it means |
| --- | --- | --- |
| `correlated` | app hot and ≥1 storage hot | That storage tier is a candidate cause |
| `storage_only` | storage hot, app within SLO | A bottleneck that has not yet reached users — the masked case |
| `app_only` | app hot, every storage probe healthy **on sufficient data** | The application tier itself |
| `client_limited` | `client_wait` dominates | The generator or its configuration; the target is not implicated |
| `unobserved` | app hot, storage probes insufficient | We cannot say, and we say that |

`app_only` requires the probes to have had enough data to be believed. Without that
requirement, a quiet probe would produce a confident accusation against the
application tier, which is the most damaging possible failure mode for this tool:
`unobserved` exists precisely so the tool can decline to answer.

`client_limited` is checked first. Blaming a target for the generator's own queueing
would discredit every other finding.

### 3. Culprit ranking inside a correlated incident

Ranked on three signals, in this order of weight:

1. **Lead time** — how many buckets the tier went hot before the application did.
   Cause precedes effect; a tier that moved first is the better candidate.
2. **Severity** — peak p99 divided by that tier's threshold, which normalises a
   database at 400ms against a cache at 40ms.
3. **Telemetry corroboration** — pool saturation, lock waits, blocked clients, a
   cache hit-ratio collapse. This is the only signal that is not latency, so it is
   what lifts a correlation into corroborated evidence.

**Ties are reported as ties.** When two tiers score equally the incident names both
and the digest says so. Breaking a tie arbitrarily would manufacture confidence the
data does not support.

### 4. Whole-run lagged correlation

Spearman's rank correlation between the application p99 series and each storage p99
series, at lags of -3 to +3 buckets. Spearman rather than Pearson because the
relationship between tiers is monotonic but not linear — a queue's response to load is
convex — and because ranks are robust to the outliers that dominate a latency series.
The best lag is reported alongside rho: a positive lag means the storage tier moved
first, which is what one expects if it is the cause. A high rho at lag 0 across a
whole run is weaker evidence than it looks, since both series respond to the same load
ramp; the incident-level analysis carries more weight, and the confidence rubric
reflects that.

### 5. Verdicts are templated, never generated

The narrative is assembled from fixed sentence templates filled with measured numbers.
Same result in, same words out — which is what makes golden-file tests possible and
what stops a verdict drifting between two readings of one file. Every verdict carries
structured evidence that a reader can check against `result.json` itself, a confidence
level, the standing caveat that shared-clock correlation shows association rather than
causation, and concrete next steps.

Confidence is a rubric, not a judgement: **high** needs sufficient samples throughout,
at least two consistent incidents or |rho| ≥ 0.7, and telemetry corroboration;
**medium** drops one of those; **low** is anything thinner, and a low-confidence
verdict is explicitly marked as not a finding.

### 6. Validity gates everything

Before any of the above is worth reading, the run itself must be sound.

- **invalid**: dispatch lag p99 above `max(5ms, 5% of a bucket)` — the generator fell
  behind its own schedule, so every latency number includes our delay — or dropped
  arrivals above 1% *while service time stayed flat*, which means a client-side cap,
  not a target limit.
- **degraded**: dropped arrivals *with rising* service time (`target_saturated`, a
  real finding about the target); loopback targets (the same-host caveat, detected
  from resolved addresses — generator and target competing for one machine's CPU);
  over 5% HTTP 429 ("you are measuring the rate limiter"); insecure TLS; telemetry
  unavailable; a Little's Law inconsistency over 20% between measured concurrency and
  `rate x service time`.

An invalid run exits 4 and its verdict must not be interpreted. This is why validity
is the first field in the digest and the first thing the bundled skill teaches.

## Consequences

- Every threshold above is a constant in one place, documented in
  `docs/METHODOLOGY.md` with this reasoning, and overridable.
- The known-answer suite (§10) is what keeps this honest: injected faults with known
  causes must produce the matching class, within ±1 bucket, five times over in nightly
  CI. A methodology that cannot pass its own fault injection is not a methodology.
- The tool will sometimes answer `inconclusive` or `unobserved`. That is a correct
  answer and is treated as one, not as a failure to be tuned away.

## Alternatives considered

- **Pearson correlation on raw latencies.** Assumes linearity that queueing systems do
  not have, and is dominated by the outliers that matter most.
- **Standard-deviation outlier detection.** The outliers inflate the statistic that is
  supposed to detect them.
- **Machine-learned anomaly detection or an LLM-written narrative.** Non-deterministic
  and unexplainable. A verdict a user cannot audit against the numbers is worse than
  no verdict, and both would break golden-file testing.
- **Claiming causation when correlation is strong.** The tool has no causal evidence.
  Saying so in every verdict is a feature.
