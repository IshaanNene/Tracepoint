# ADR-001: An in-house, coordinated-omission-correct arrival scheduler

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §2 (no load-generation libraries), §3.3 (timing model)

## Context

The whole tool rests on one claim: that a latency number describes the target rather
than the generator. Two things break that claim.

The first is **coordinated omission**. A loop that sends a request, waits for the
response and then sends the next one stops offering load exactly when the target
slows down. The slow responses are therefore under-counted, and the reported p99
flatters the target — the worse the stall, the fewer samples it contributes.
Measuring from the moment a request was *scheduled* to be sent, rather than from when
it was actually sent, restores the missing time.

The second is **schedule drift**. Sleeping `1/r(t)` between arrivals accumulates the
cost of every wake-up, every scheduler delay and every rounding error, so a nominal
200 req/s run silently becomes 197 req/s, and a ramp never reaches its target. Over
three minutes the error is large enough to move a capacity boundary.

An off-the-shelf load library would settle both questions for us, in a way we could
neither verify nor explain in a report, and the scheduler is the one piece of this
tool that has to be exactly right.

## Decision

Write the scheduler, and compute arrival times in closed form.

**The load profile is piecewise linear.** A stage carries a duration `D` and a target
rate; within a stage the rate moves linearly from the previous stage's target `r0` to
this one's `r1`. The cumulative expected number of arrivals is

```
Lambda(t) = integral of r from 0 to t
```

which within a stage, at local time `s`, is

```
Lambda(s) = a*s^2 + b*s        with a = (r1 - r0) / (2D),  b = r0
```

**Arrival k fires at `Lambda^-1(k)`.** Inverting means solving `a*s^2 + b*s = c` for
the count `c` remaining inside the stage. The textbook quadratic root
`(-b + sqrt(b^2 + 4ac)) / 2a` loses most of its significant digits when `a` is near
zero — that is, on a constant-rate stage, which is the common case — because it
subtracts two nearly equal numbers and then divides by something tiny. Rationalising
the numerator gives an algebraically identical but numerically stable form:

```
s = 2c / (b + sqrt(b^2 + 4ac))
```

This form needs no special case for a constant rate: with `a = 0` it collapses to
`s = 2c / 2b = c / r0`, which is exactly right. Two edge cases remain and are handled
explicitly: a stage whose rate is zero throughout (`a = b = 0`) produces no arrivals
and is skipped, and a count beyond a stage's capacity belongs to a later stage, so
the search moves on before inverting.

**Arrival processes.** `uniform` feeds the integers `k = 1, 2, 3, ...` into
`Lambda^-1`, spacing arrivals evenly along the profile. `poisson` feeds cumulative
sums of `Exp(1)` draws into the *same* inverse. That is the time-rescaling theorem:
rescaling a rate-1 Poisson process by `Lambda^-1` yields a non-homogeneous Poisson
process with intensity `r(t)`. One inverse serves both, and the mean rate is
identical — only the clustering differs. Poisson is the better model of independent
users and produces the bursts that find queueing problems; uniform is the default
because it is easier to reason about when reading a chart.

**Every timestamp is an offset** from a single monotonic run start shared by every
runner and sampler. Bucket index is `floor(intended_offset / bucket_width)` — the
*intended* time, not the completion time, so a request that takes four seconds is
still counted against the second it was meant to be sent. Wall-clock time is recorded
once, for display.

**Each outcome carries six stamps**: intended, dispatched, worker-start,
connection-acquired, first-byte (HTTP only) and end. From them:

```
response_time = end - intended              what a user feels
service_time  = end - connection_acquired   what the target did
client_wait   = dispatch lag + queue wait + pool wait
```

`response_time` is what SLOs are judged on, because it is what a user would
experience. `service_time` is what tier attribution uses, because blaming a database
for time spent queueing inside our own process would be wrong. `client_wait` is the
difference, and it is reported rather than hidden: when it dominates a hot bucket the
finding is about the generator, and the run says so instead of blaming the target.

## Consequences

- The scheduler is testable without a network: given stages and a fake clock, the
  arrival sequence is deterministic and can be checked against `integral of r`.
  Property tests assert arrival counts within ±1 of the integral for random stage
  sequences and that `Lambda^-1` is monotonic.
- A linear ramp to `R` over `T` must yield `R*T/2` arrivals. This is the standing
  sanity check, cheap enough to assert in a test and specific enough to catch an
  algebra mistake.
- We own the correctness of the timing model, and can explain it in
  `docs/METHODOLOGY.md` rather than pointing at someone else's source.
- `float64` carries roughly 15 significant digits; at an hour-long run and
  microsecond resolution the worst-case representation error is nanoseconds, far
  below the millisecond scale everything is reported at.

## Alternatives considered

- **A load-generation library (vegeta, k6's engine, ghz).** Ruled out by §2, and
  rightly: the scheduler is the part of this tool whose exact behaviour we most need
  to describe and defend.
- **Sleep `1/r(t)` between arrivals.** Simple, and drifts. Rejected in the spec.
- **Pre-materialise every arrival time into a slice.** Exact, but allocates in
  proportion to `rate x duration` — 2k req/s for ten minutes is 1.2M entries — and
  the closed form gives the same answer in constant space.
- **Measure latency from dispatch rather than from the intended time.** That is
  coordinated omission by another name: it hides exactly the delay the tool exists to
  find.

## Verification performed at decision time

The formula was checked numerically before being adopted, not assumed. Each of these
becomes a Go test in phase 1.

| Check | Result |
| --- | --- |
| Near-constant stage (`r0=200`, `r1=200.000001`, `D=150`), counts 1 … 29999 | stable form residual `a*s^2+b*s-c` is 0 to 1.4e-14; the naive root's residual is 8e-6 to 2e-4 |
| Exact constant stage (`a=0`) | stable form returns `c/r0` with no special case; the naive root divides by zero |
| Ramp identity: `Lambda(T)` for `0 -> R` over `T` | exactly `R*T/2` for `(200,30s)`, `(500,60s)`, `(1000,12.5s)` |
| End-of-stage identity: invert `c = Lambda(D)` | returns `D` with zero error for ramp-up, hold, ramp-down and fractional-rate stages |
| Monotonicity across a 3-stage profile (ramp 0→200 over 30s, hold 150s, ramp→0 over 30s) | 36000 arrivals, strictly increasing, first at 0.547723s (`= sqrt(1/a)`), last at 210.000000s |
