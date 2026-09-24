# CLAUDE.md

Guidance for Claude Code and other coding agents working in this repository. This file
is canonical; `AGENTS.md` points here.

## Read these first, in order

1. **[`docs/SPEC.md`](docs/SPEC.md)** — the source of truth. Stored verbatim and never
   edited. Re-read the sections relevant to your phase before starting it; it is
   written to survive context compaction.
2. **[`docs/PROGRESS.md`](docs/PROGRESS.md)** — what is actually built, what deviates
   from the spec and why, and what is known to be missing.
3. **[`docs/adr/`](docs/adr/)** — every decision the spec left open, with reasoning.
   If you are about to make an architectural choice, check whether it is already made.

## What this is

TracePoint drives HTTP, SQL and Redis load concurrently on one monotonic clock and
reports — with evidence and a confidence level — whether a slowdown came from the
application tier, the database or the cache. Storage runners are probes as well as
load. Two audiences with equal priority: humans (HTML report, terminal) and agents
(JSON contracts, MCP, REST, a bundled skill, a Go API).

## Commands

```bash
make check         # the gate: fmt, vet, lint, race tests, govulncheck. Must be green to commit
make test          # unit tests with -race
make build         # static binary into bin/tracepoint
make integration   # testcontainers: postgres, mysql, redis   (tag: integration)
make e2e           # known-answer fault-injection suite        (tag: e2e, needs Docker)
make soak          # 10-minute flat-heap soak                  (tag: soak)
make bench         # record path, scheduler, sketch merge
make cover         # coverage with the gate check
make help          # all targets
```

Run a single test: `go test -race -run TestName ./internal/schedule/`.

`make integration` and `make e2e` need a running Docker daemon (testcontainers pulls
`postgres:16-alpine`, `mysql:8.4` and `redis:7-alpine`); in a fresh cloud container
start one with `dockerd &`. `make check` never needs Docker. `govulncheck` needs to
reach `vuln.go.dev`. The known-answer suite takes about five minutes.

The HTML report has a real-browser test (`TestInABrowser` in `internal/render/html`):
it runs headless Chromium - found under `/opt/pw-browsers`, on `PATH`, or at
`$TRACEPOINT_CHROMIUM` - and skips when there is none. Golden files are rewritten
with `go test ./<pkg>/ -update`; review the diff before committing it.

## Layout

`cmd/tracepoint` is wiring only. The public Go API is at the module root
(`tracepoint.go`, `options.go`); everything else is under `internal/`. Adapters — CLI,
MCP, REST, the GitHub Action — reach the core **only** through the public API and hold
no business logic. Full package map: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Conventions that are not negotiable

These come from §2 of the spec. Breaking one is a bug, not a style preference.

- **`result.json` is the system of record.** Every renderer — CLI, HTML, Markdown,
  JUnit, digest — is a pure function of it. If a number is not in the schema, it
  cannot appear anywhere. Add the field to `internal/schemas/result.schema.json` first.
- **Never average percentiles.** Merge sketches and read the merged result.
- **Measure from the intended send time**, not from dispatch. `response_time` is what
  SLOs are judged on; `service_time` is what tier attribution uses; `client_wait` is
  the generator's own delay and is always reported, never hidden.
- **Inject everything**: `Clock`, a seeded RNG (the seed is recorded), `*slog.Logger`,
  stdout and stderr, filesystem roots. No mutable package-level state.
- **`context.Context` on every blocking call.** `goleak` runs in every package's
  `TestMain`.
- **Errors wrap with `%w`** and carry a stable code from
  [`docs/ERRORS.md`](docs/ERRORS.md). No panics outside `main`.
- **The hot path allocates nothing** and takes no global lock: per-worker shards,
  merged on read. It is benchmarked.
- **Contract-first.** Config, policy, result, digest, events and errors each have a
  JSON Schema in `internal/schemas/`. Every emitted document is validated against its schema in
  tests. Changing a schema means changing `docs/PROGRESS.md` and, if it is not purely
  additive, a version bump — see [ADR-004](docs/adr/004-result-system-of-record.md).
- **Math-heavy packages are written test-first**: `schedule`, `metrics`, `analysis`,
  `capacity`, `compare`.
- **No feature is done until its agent interface is done.** A capability reachable
  only from an interactive terminal is unfinished.
- **Never claim something works without running it.** If the spec conflicts with
  reality, stop and present options with trade-offs rather than quietly deviating.

## Naming

The spec's working name is `strata`; the tool is named **`tracepoint`**
([ADR-010](docs/adr/010-name.md)). `docs/SPEC.md` is the only file permitted to contain
the old name, because §0.1 requires it stored verbatim, and CI enforces that.

## Working rhythm (§0.3)

Each phase ends with: `make check` green → self-review against that phase's acceptance
criteria **with evidence** (test names, command output) → update `docs/PROGRESS.md` →
small conventional commits → a brief report of what was done, what deviated and what
is risky → wait for approval before the next phase.

Commit style: conventional commits (`feat(schedule): ...`, `fix(metrics): ...`,
`docs(adr): ...`, `chore: ...`). Small and focused.

## Clean room

Implement from `docs/SPEC.md` only. Do not copy code from any other repository.
Vendored third-party assets keep their `LICENSE` files.
