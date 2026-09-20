# Architecture

> Source of truth is [`docs/SPEC.md`](SPEC.md). Decisions the spec leaves open are
> recorded in [`docs/adr/`](adr/). Current state is [`docs/PROGRESS.md`](PROGRESS.md).

## The product, in ten lines

1. When an application slows down under load, TracePoint answers one question: was it
   the application tier, the database, or the cache?
2. It drives HTTP traffic (single requests or multi-step journeys), SQL and Redis load
   concurrently, all on one monotonic clock.
3. Storage runners are probes as well as load: they hit the datastore directly, so
   their latency measures that tier's health while the application is under pressure.
4. Every layer records into the same time buckets, so latencies from three tiers are
   directly comparable at every instant of the run.
5. Server-side telemetry — Postgres, MySQL, Redis, and the generator's own health — is
   sampled on that same clock to corroborate what the latencies suggest.
6. Analysis merges abnormal buckets into incidents, classifies each one, ranks a
   culprit, and issues a verdict with evidence and a confidence level.
7. It states that correlation is not proof, every time, and answers "inconclusive"
   when the data cannot support more.
8. It knows when the run itself was the problem: a generator that fell behind makes a
   run invalid, and an invalid run's verdict must not be interpreted.
9. Humans get an offline HTML report, a live terminal view and readable tables; agents
   get stable JSON contracts, an MCP server, a REST API, a bundled skill and a Go API.
10. Neither audience is the wrapper: a feature is not done until its agent interface is
    done.

## Shape

Hexagonal. One core, thin adapters, and no business logic outside the core.

```
                    ┌──────────────── adapters (no business logic) ────────────────┐
   terminal ──────► │ CLI (cobra)                                                  │
   Claude/agents ─► │ MCP server ──┐                                               │
   services ──────► │ REST server ─┤ generated from internal/ops registry          │
   CI ────────────► │ GitHub Action│                                               │
   Go tests ──────► │ tracepointtest                                               │
                    └───────────────────────────┬──────────────────────────────────┘
                                                │  public Go API only (ADR-009)
                    ┌───────────────────────────▼──────────────────────────────────┐
                    │ tracepoint.{LoadConfig, Plan, Run, Compare, Digest}           │
                    └───────────────────────────┬──────────────────────────────────┘
                                                │
   config ──► policy ──► preflight ──► ┌────────▼────────┐ ──► result.json ──► analysis
                                       │  engine         │         │
                                       │  clock          │         └──► renderers
                                       │  schedule (Λ⁻¹) │              cli · html
                                       │  executor       │              markdown · junit
                                       │   ├ runners     │              digest
                                       │   └ samplers    │
                                       │  metrics        │
                                       └─────────────────┘
```

## The one-way flow

Configuration is loaded, interpolated, strictly decoded, validated and clamped by
policy — and from then on it does not change. Preflight resolves and classifies every
target, connects, and prepares every statement *without executing it*. Only then does
load start.

During the run there is exactly one writer of truth: the metrics recorder, fed by
per-worker shards that never contend. Buckets seal on a timer, reduce to fixed-size
summaries and free their sketches (ADR-002), so memory does not grow with run length.

At the end, everything collected becomes `result.json` (ADR-004). **Every renderer is
a pure function of that file** — the terminal tables, the HTML report, the Markdown
summary, the JUnit XML, the agent digest and `compare` alike. Nothing reads live engine
state, which is why `tracepoint report <result.json>` reproduces exactly what the run
printed, and why a number absent from the schema cannot appear anywhere.

Analysis (§5.5) is likewise pure: given a result, the same verdict comes out every
time, in the same words. That is what makes it testable and what makes it auditable.

## Package map

| Package | Holds |
| --- | --- |
| `cmd/tracepoint` | `main`: wiring only — build the dependency graph, hand it to the command tree, map the error to an exit code |
| `tracepoint`, `options.go` | Public, semver-stable Go API (ADR-009) |
| `tracepointtest` | `Run(t, cfg)` — fails a Go test on an SLO breach or an invalid run |
| `internal/errs` | Coded errors and the JSON envelope. Sits below everything, because every package constructs one |
| `internal/engine` | Orchestration: owns the single monotonic start instant every runner and sampler shares |
| `internal/cli` | The cobra command tree. An adapter: it parses flags, calls the core and maps outcomes to exit codes |
| `internal/clock` | `Clock` interface, real and fake. Nothing else reads `time.Now` |
| `internal/schedule` | Rate stages, `Λ` and `Λ⁻¹`, uniform and Poisson arrivals (ADR-001) |
| `internal/executor` | Open (arrival-rate) and closed (vus) models (ADR-003) |
| `internal/config` | Load, interpolate, strict-decode, default, validate, `--set`, redact |
| `internal/policy` | The human-granted envelope; tighten-only merge; target classification (ADR-006) |
| `internal/template` | The mini-template language, compiled once, with a static dataflow check |
| `internal/metrics` | Outcome model, error taxonomy, sketches, sharded recorders, bucket sealing (ADR-002) |
| `internal/telemetry` | Samplers: generator, Postgres, MySQL, Redis |
| `internal/analysis` | Hot buckets, incidents, culprits, lagged correlation, verdicts, validity, SLOs, strain (ADR-005) |
| `internal/capacity` | Auto-ramp search, boundary confirmation, USL fit |
| `internal/compare` | Quantile confidence intervals, gates, incident diff |
| `internal/result` | The result model, schema versions, migrations, digest builder (ADR-004) |
| `internal/runstore` | Run directories, state, events, detach/status/wait/stop, audit log |
| `internal/ops` | The one operation registry; MCP, REST and `capabilities` are generated from it (ADR-008) |
| `internal/adapters/{mcp,rest}` | Protocol plumbing, no logic |
| `internal/render/{cli,html,markdown,junit}` | Pure functions of `result.json` |
| `internal/detect` | Project detection and OpenAPI import |
| `internal/schemas` | The JSON Schemas themselves. They live inside the package because `go:embed` cannot reach outside it, which is what guarantees the binary ships the contract it enforces |
| `internal/buildinfo` | Version stamped at link time |
| `tools/faultbox` | Demo app with fault injection, for the known-answer suite |

## Extension points — exactly three

No speculative abstraction. The three registries named in the spec, and nothing else:

- **Runner registry** — `Name`, `Kind`, `Prepare`, `Do`, `Close`. A gRPC, Kafka or
  Mongo runner can be added without touching the engine.
- **Sampler registry** — `Name`, `Sample(ctx, at)`. New telemetry sources plug in on
  the shared offset clock.
- **Operation registry** — one entry yields an MCP tool, a REST endpoint and a
  `capabilities` entry (ADR-008).

## Concurrency

One monotonic run start is shared by every runner and sampler. The executor owns
dispatch; runners own their pools; the recorder owns the sharded write path. Every
blocking call takes a `context.Context`, cancellation is honoured throughout, and
`goleak` runs in every package's `TestMain` — a leaked goroutine in a load generator
is a memory leak with a schedule.

Injected everywhere, never global: `Clock`, a seeded RNG (seed recorded in the
result), a `*slog.Logger`, stdout and stderr, and filesystem roots. No mutable
package-level state, which is what makes a run reproducible from its recorded seed.

## Deliberately out of scope

Named in §14 so the design accommodates them without building them: browser and
end-to-end testing, WebSockets and streaming, distributed multi-node generation (the
engine stays coordinator-friendly — one monotonic start, mergeable sketches and
offset-based buckets are exactly what a coordinator would need), gRPC/Kafka/Mongo
runners (the registry is ready), and live Prometheus or OTLP export.

**The natural v2 is OpenTelemetry trace ingestion.** Everything in §5.5 is
correlational because timing is all this tool has. Joining a run against the target's
own traces — TracePoint already injects W3C `traceparent` and tags database sessions
for exactly this reason — would turn "these moved together" into per-request causal
attribution. That is a different and much stronger claim, and it is the one thing that
would materially improve the answer rather than broaden it.
