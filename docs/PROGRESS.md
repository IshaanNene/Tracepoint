# Progress

> Read [`docs/SPEC.md`](SPEC.md) first — it is the source of truth. This file records
> what is actually built, what deviates, and what is known to be missing. It is updated
> at the end of every phase, before the commits for that phase.

## Status

| Phase | Scope | State |
| --- | --- | --- |
| 0 | Plan: spec stored, ADRs, JSON Schemas, docs, skeleton, Makefile, lint, CI stub | **Complete — awaiting approval** |
| 1 | Engine slice: clock, schedule, sketches, recorder, arrival-rate executor, HTTP runner, `result.json` v1, CLI table, JSON output, exit codes | Not started |
| 2 | Storage and safety: SQL and Redis runners, templates, `--set`, policy, guards, redaction, preflight, `doctor`, `--dry-run` | Not started |
| 3 | Analysis: telemetry samplers, incidents, culprits, correlation, verdicts, validity, digest; faultbox and the known-answer suite | Not started |
| 4 | Agent interfaces: run store, detach, events, audit log, ops registry, `capabilities`, MCP, REST, parity tests | Not started |
| 5 | Human interfaces: HTML, Markdown, JUnit, `report`, TUI | Not started |
| 6 | Journeys and onboarding: vus executor, extract/expect, feeders, think time, cookies, strain finder, `quick`, `init` | Not started |
| 7 | Capacity and compare: auto-ramp, USL, confidence intervals, gates, incident diff | Not started |
| 8 | Integration and release: Go facade, `tracepointtest`, Action, Docker, skill, contract CI, fuzzing, soak, GoReleaser, agent dogfood | Not started |

## Phase 0 — complete

### Delivered

| Deliverable | Where | Evidence |
| --- | --- | --- |
| Spec stored verbatim (§0.1) | `docs/SPEC.md` | 362 lines, byte-for-byte as supplied |
| Product restated in ten lines (§12.0) | `docs/ARCHITECTURE.md` | Top section |
| Assumptions and open questions (§12.0) | `docs/ASSUMPTIONS.md` | 12 assumptions, 7 open questions, each with a default already applied |
| Architecture | `docs/ARCHITECTURE.md` | Flow, package map, three extension points, concurrency, v2 note |
| ADRs 001–009 plus 010 | `docs/adr/` | 10 records, indexed in `docs/adr/README.md` |
| JSON Schemas (config, policy, result, digest, events, error) | `schemas/*.json` | All six pass Draft 2020-12 meta-validation |
| Schema fixtures | `schemas/testdata/config/{valid,invalid}/` | 9 valid, 16 invalid, each verified to land on the side its directory claims |
| `CLAUDE.md` canonical, `AGENTS.md` pointing to it (§0.4) | repo root | |
| Skeleton, Makefile, lint config, CI stub (§12.0) | 28 packages, `Makefile`, `.golangci.yml`, `.github/workflows/` | `go build`, `go vet` and `golangci-lint run` all clean |

### Evidence

```
$ go build ./... && go vet ./...          # clean
$ golangci-lint run                        # 0 issues
$ gofmt -s -l .                            # no output
$ tracepoint version                       # emits the buildinfo JSON document
```

Schema contract checks, all passing:

- All six schemas pass `Draft202012Validator.check_schema`.
- The spec's own illustrative config (§4) validates against `config.schema.json` with
  **zero** errors — the strongest available evidence that the finalised key names are
  faithful to the spec.
- 16 malformed configs are each rejected for the right reason: unknown field,
  `requests` and `journeys` together, no runner, zero weight, malformed duration,
  `rate` with `stages`, `vus` executor given `rate`, a query with neither or both of
  `sql`/`tx`, an unknown driver, Redis `addr` with `addrs`, `error_rate` above 1,
  `body` with `body_file`, an unsupported version, a bad `refine` expression, and a
  non-identifier extract name.

ADR-001's formula was verified numerically before adoption, not assumed — see the
verification table in that record. Summary: the stable root has zero residual where the
naive quadratic root carries 1e-4 error; `a = 0` collapses to `c/r0` with no special
case; `Lambda(T) = R*T/2` holds exactly; inverting `Lambda(D)` returns `D` exactly; and
`Lambda^-1` is strictly monotonic across 36,000 arrivals of a three-stage profile.

### Deviations from the spec

| Deviation | Reason |
| --- | --- |
| The working name `strata` is rendered `tracepoint` throughout | §preamble instructs exactly one rename; the title and repository both say TracePoint. [ADR-010](adr/010-name.md). `docs/SPEC.md` keeps the original word because §0.1 requires it verbatim, and a CI check makes that the only file allowed to |
| Per-label bucket timelines are retained under a budget rather than unconditionally | §3.4 asks for per-(runner,label,bucket) summaries *and* a flat heap across a ten-minute soak; at 50 labels these conflict. No analysis reads per-label timelines. [ADR-002](adr/002-sketches-and-memory.md), assumption A3 |
| `go.mod` has no third-party modules yet | Phase 0 is planning; every dependency is pre-justified in [ADR-007](adr/007-dependencies.md) and added by the phase that needs it |

### Risks carried into phase 1

| Risk | Mitigation |
| --- | --- |
| The schemas are drafted before the code exists, so some field will be wrong | They are versioned from the start and additive changes are minor (ADR-004). Each phase validates its emitted documents against them, which is what surfaces a wrong field early |
| `0 allocs/op` on the record path is a strong claim | Benchmarked in phase 1, not asserted. If a sketch implementation allocates per observation it is caught there, while the choice is still cheap to revisit |
| Golden-file tests plus a live TUI can make `make check` flaky | Renderers are pure functions of `result.json` (ADR-004) and the TUI is TTY-only; golden tests never touch the terminal path |
| `govulncheck` and `goreleaser` are not installed locally | `make check` fails with the install command rather than skipping. CI installs both |
