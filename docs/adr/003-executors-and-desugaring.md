# ADR-003: Two executors; `requests` desugars to single-step journeys

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §3.3 (open model), §4 (requests or journeys, never both), §5.2 (executors)

## Context

Two load models answer different questions, and conflating them produces numbers that
look comparable and are not. Meanwhile the HTTP runner has two surface syntaxes —
weighted single requests and multi-step journeys — and implementing both would double
the code that handles extraction, expectations, cookies and think time.

## Decision

### Two executors, chosen explicitly

**`arrival-rate` (open model)** fires arrivals on the schedule from ADR-001 regardless
of whether earlier ones have finished. Offered load is an input, not an outcome. This
is what makes latency coordinated-omission correct, and it is the default, because
"how does the system behave at 200 req/s" is the question people actually have.

**`vus` (closed model)** runs a fixed population of virtual users, each looping as
fast as the target allows. Offered load is an *output*: when the target slows, the
population automatically offers less, exactly as a bounded set of real users with
sequential sessions would. This models a fixed client pool honestly, and it is the
natural fit for long journeys. `pick: per-iteration` (default) re-draws a weighted
journey each loop; `per-vu` pins each user to one journey for the run, which is right
when journeys model distinct user types.

Both executors feed the same runners and produce the same outcome records, so nothing
downstream has to know which ran.

### Backpressure is bounded and counted, never silent

The open executor puts a bounded queue in front of `max_in_flight` workers. When every
worker is busy and the queue is full, the arrival is **dropped and counted** — it is
never held indefinitely. An unbounded queue would turn a capacity problem into a
memory problem and inflate `response_time` without limit, reporting the generator's
own backlog as the target's latency.

Drops are therefore first-class data, not an error. `dropped_ratio` above 1% is
reported, and §5.5 reads it two ways: with flat service time it means the generator
was capped and the run is **invalid**; with rising service time it means the target
was saturating and the run is **degraded** with a `target_saturated` finding. The same
number, opposite conclusions — which is precisely why it must be recorded rather than
smoothed away.

Defaults: `max_in_flight` 256, `queue_depth` 64. Little's Law sizes the first
properly (`in flight ≈ rate x p99 service time`), and when a run is client-limited the
digest recommends the computed value as an applicable override.

### `requests` desugars to single-step journeys

`http.requests` is sugar. At load time each request becomes a one-step journey with
the same name and weight, and one app engine runs journeys only. Extraction, cookie
jars, expectations, think time and per-step timeouts then have exactly one
implementation. The desugaring is invisible in output: labels stay the request names,
so a user who wrote `requests` never sees the word "journey".

Both keys in one config is a load-time error (§4) rather than a silent merge: the two
imply different intents, and guessing would be worse than asking.

### Stopping

Arrivals stop at the end of the profile; in-flight work drains for up to `grace`, then
the context is cancelled and anything still running is recorded as `canceled` — a
distinct error class, so shutdown noise is never mistaken for target failure. The
first SIGINT triggers the same graceful path and writes a partial result marked
`interrupted`; a second exits immediately. Warm-up is excluded from every summary,
baseline and SLO check, and is shaded in charts rather than hidden, so a reader can
see what was left out.

## Consequences

- The executor is the only component that knows about time-to-send; runners receive an
  iteration and record against it, which keeps the runner interface small enough that
  gRPC or Kafka runners can be added later without touching the engine.
- `vus` cannot be coordinated-omission corrected, because in a closed model there is
  no scheduled send time to measure from — the previous response *is* the schedule.
  This is a property of the model, not a defect, and the methodology document says so;
  `response_time` and `service_time` converge under `vus`.
- Capacity search (§5.6) drives either executor through the same knob abstraction:
  `rate` for open, `concurrency` for closed.

## Implementation notes (phase 6)

Choices the decision above left open, made when the closed model and journeys were
built. The rules themselves are in [`METHODOLOGY.md`](../METHODOLOGY.md).

- The `vus` executor re-reads its target user count every 100ms and rounds it. Users
  retire between iterations, never mid-journey.
- The first step of a journey is timed from the iteration's intended send time and
  later steps from their actual start, so lateness is charged once.
- Think time runs between steps only; a pause after the last step would idle a
  closed-model user without modelling anything.
- Cookie jars follow the model: one per virtual user for the run under `vus`; one per
  iteration under `arrival-rate`, and none at all for single-step journeys, which
  cannot carry a cookie anywhere. `http.cookies: false` turns them off.
- A `unique` feeder that runs out fails the step as `extract_failed`: the step cannot
  be given the value its configuration promised, which is exactly that class.

## Alternatives considered

- **Open model only.** Simpler, and wrong for a fixed client pool: it would report a
  connection-limited service as failing an SLO it could never breach in production.
- **Unbounded dispatch queue.** Loses the distinction between "the target is slow" and
  "we could not ask fast enough", which is a distinction this tool exists to make.
- **Keep `requests` and `journeys` as separate engines.** Two implementations of
  extraction and expectations, and two places for a bug to hide in exactly the logic
  that decides whether an operation counts as an error.
