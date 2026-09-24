# TracePoint

**When your app slows down under load, was it the app, the database, or the cache?**

TracePoint drives HTTP traffic, SQL queries and Redis commands *concurrently*, on one
monotonic clock, and records every layer into the same time buckets. The storage
runners are probes as well as load: they hit the datastore directly, so their latency
is a direct measurement of that tier's health while your application is under
pressure. When application latency moves with a probe's latency — and the datastore's
own telemetry agrees — that is evidence about which tier is responsible.

It reports a verdict with the evidence behind it and a confidence level, and it says
plainly that correlation is not proof.

```
  INCIDENTS 1
    ID     CLASS       WINDOW   HOT      CULPRIT  APP p99
    inc-1  correlated  20s-26s  http,db  db       5.1s
    · inc-1: postgres locks_waiting 0 -> 301

  VERDICT   DB  confidence: medium
    The database tier is the most likely source of the slowdown. In the one
    incident that reached users, covering 6s, application latency rose together
    with the db probe, which went hot in the same bucket. Server-side telemetry
    corroborated it: postgres locks_waiting, postgres sessions_waiting_lock and
    postgres sessions_active moved in the same window.
    Shared-clock correlation shows association, not causation.
```

That is real output, trimmed, from the known-answer scenario that locks a Postgres
table for five seconds starting 20 seconds in. Confidence is medium rather than high
because the target shared the generator's machine, and the report says so.

It is built for two audiences with equal priority. Humans get an offline HTML report,
a live terminal view and readable tables. Agents and automation get stable JSON
contracts, an MCP server, a REST API, a bundled skill, a Go library and CI
integrations — every capability reachable non-interactively with machine-readable
input and output.

## Status

**Phase 5 of 8 complete: the engine, the analysis, and both the agent and the human
interfaces work.** TracePoint runs HTTP, SQL (Postgres, MySQL, SQLite) and Redis load
on one clock, samples the datastores' own telemetry, and reports incidents, a verdict
with evidence and confidence, and a context-sized digest for agents. A known-answer
suite injects faults into a demo application and checks that every verdict class comes
out right. Agents drive it over MCP, REST or the shell; people get a live terminal
view and an offline HTML report for every run, with Markdown and JUnit one command
away. Journeys, capacity search and `compare` are the phases still to come, and there
is no release yet.

| What exists | Where |
| --- | --- |
| The specification, stored verbatim | [`docs/SPEC.md`](docs/SPEC.md) |
| What is built, and what deviates | [`docs/PROGRESS.md`](docs/PROGRESS.md) |
| How a verdict is reached, with every constant | [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md) |
| Architecture and the package map | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| Ten architecture decision records | [`docs/adr/`](docs/adr/) |
| JSON Schemas: config, policy, result, digest, events, error | [`internal/schemas/`](internal/schemas/) |
| Error and finding codes | [`docs/ERRORS.md`](docs/ERRORS.md) |

The README gains its 30-second quickstart in phase 8.

## What it does, and will do

- **Coordinated-omission-correct timing.** Latency is measured from the moment a
  request was *scheduled* to be sent, not from when it actually went out, so a
  target's stall cannot hide by throttling the generator. Client-side waiting is
  measured separately and reported, never charged to the target.
- **Incidents, not spike rows.** Abnormal buckets merge into episodes, and each is
  classified: `correlated`, `storage_only` (a bottleneck users cannot feel yet),
  `app_only`, `client_limited` (our fault, not yours) or `unobserved`.
- **Server-side telemetry** from Postgres, MySQL and Redis on the same clock, to
  corroborate what the latencies suggest.
- **Run validity.** A run where the load generator was the bottleneck is marked
  **invalid** and its verdict must not be interpreted. The tool would rather tell you
  the measurement failed than hand you a confident wrong answer.
- **Statistically sound comparison.** `compare` uses distribution-free confidence
  intervals on quantiles, so it does not report noise as a regression — and exits
  non-zero for CI when it does find one.
- **Capacity search** with bisection, boundary confirmation and a Universal
  Scalability Law fit.
- **Safe by default.** Public targets are refused unless a human allowlists them;
  writes and destructive statements need explicit grants; secrets are redacted from
  every artifact; the HTML report makes zero network requests.

## Building

```bash
make build      # static binary into bin/tracepoint
make check      # the gate: format, vet, lint, race tests, contracts, vulnerabilities
make e2e        # the known-answer suite against real Postgres and Redis (needs Docker)
make help       # every target
```

Requires Go 1.26.6 or later. Release builds are `CGO_ENABLED=0` and cross-compile to
linux, darwin and windows on amd64 and arm64.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). If you are a coding agent, start with
[`CLAUDE.md`](CLAUDE.md).

## Licence

MIT — see [`LICENSE`](LICENSE).
