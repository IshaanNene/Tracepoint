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
VERDICT   db  ·  confidence: high
  Application p99 rose from 34ms to 780ms across 3 incidents. In each, the Postgres
  probe went hot 1 bucket earlier (peak 612ms against a 100ms threshold) while the
  Redis probe stayed healthy. pg_stat_activity showed 14 sessions waiting on locks
  during the same windows. Spearman rho 0.84 at lag +1.
  Correlation is not proof — confirm by profiling the queries listed below.
```

It is built for two audiences with equal priority. Humans get an offline HTML report,
a live terminal view and readable tables. Agents and automation get stable JSON
contracts, an MCP server, a REST API, a bundled skill, a Go library and CI
integrations — every capability reachable non-interactively with machine-readable
input and output.

## Status

**Phase 0 of 8: planning complete, awaiting approval.** The contracts, architecture
and decisions are written; the engine is not. Nothing is installable yet.

| What exists | Where |
| --- | --- |
| The specification, stored verbatim | [`docs/SPEC.md`](docs/SPEC.md) |
| Architecture and the package map | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| Ten architecture decision records | [`docs/adr/`](docs/adr/) |
| JSON Schemas: config, policy, result, digest, events, error | [`schemas/`](schemas/) |
| Error and finding codes | [`docs/ERRORS.md`](docs/ERRORS.md) |
| Assumptions and open questions | [`docs/ASSUMPTIONS.md`](docs/ASSUMPTIONS.md) |
| What is built, and what deviates | [`docs/PROGRESS.md`](docs/PROGRESS.md) |

Follow [`docs/PROGRESS.md`](docs/PROGRESS.md) for the current state. The README gains
its 30-second quickstart in phase 8.

## What it will do

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
make help       # every target
```

Requires Go 1.26.1 or later. Release builds are `CGO_ENABLED=0` and cross-compile to
linux, darwin and windows on amd64 and arm64.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). If you are a coding agent, start with
[`CLAUDE.md`](CLAUDE.md).

## Licence

MIT — see [`LICENSE`](LICENSE).
