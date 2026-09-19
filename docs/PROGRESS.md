# Progress

> Read [`docs/SPEC.md`](SPEC.md) first — it is the source of truth. This file records
> what is actually built, what deviates, and what is known to be missing. It is updated
> at the end of every phase, before the commits for that phase.

## Status

| Phase | Scope | State |
| --- | --- | --- |
| 0 | Plan: spec stored, ADRs, JSON Schemas, docs, skeleton, Makefile, lint, CI stub | Complete, approved |
| 1 | Engine slice: clock, schedule, sketches, recorder, arrival-rate executor, HTTP runner, `result.json` v1, CLI table, JSON output, exit codes | **Complete — awaiting approval** |
| 2 | Storage and safety: SQL and Redis runners, templates, `--set`, policy, guards, redaction, preflight, `doctor`, `--dry-run` | Not started |
| 3 | Analysis: telemetry samplers, incidents, culprits, correlation, verdicts, validity, digest; faultbox and the known-answer suite | Not started |
| 4 | Agent interfaces: run store, detach, events, audit log, ops registry, `capabilities`, MCP, REST, parity tests | Not started |
| 5 | Human interfaces: HTML, Markdown, JUnit, `report`, TUI | Not started |
| 6 | Journeys and onboarding: vus executor, extract/expect, feeders, think time, cookies, strain finder, `quick`, `init` | Not started |
| 7 | Capacity and compare: auto-ramp, USL, confidence intervals, gates, incident diff | Not started |
| 8 | Integration and release: Go facade, `tracepointtest`, Action, Docker, skill, contract CI, fuzzing, soak, GoReleaser, agent dogfood | Not started |

## Phase 1 — complete

The engine works end to end: a configuration goes in, load is generated on a
coordinated-omission-correct schedule, every layer is recorded on one clock, and a
schema-valid `result.json` comes out with an exit code that means what §6.1 says.

### Delivered

| Deliverable | Where | Evidence |
| --- | --- | --- |
| Clock abstraction, real and fake | `internal/clock` | 92% coverage; `BlockUntil` lets timing tests wait for goroutines to register rather than sleeping |
| Closed-form arrival scheduler (ADR-001) | `internal/schedule` | 95% coverage, written test-first, rapid properties for arrival counts against the integral and for monotonicity; ~8ns per arrival, no per-arrival allocation |
| Sketches, sharded recording, bucket sealing (ADR-002) | `internal/metrics` | 93% coverage, written test-first, rapid properties for merge-order independence and quantile accuracy; **0 allocs/op** on the record path |
| Arrival-rate executor (ADR-003) | `internal/executor` | 93% coverage; drops are counted, never queued without bound; two contexts so a graceful stop drains in-flight work |
| Strict configuration with §6.2-quality errors | `internal/config` | 78% coverage; every unknown field reported at once with line, column and a did-you-mean |
| HTTP runner with httptrace phase timing | `internal/runner/httprun` | Weighted requests, status and transport-error classification, identifiable traffic, always-drained bodies |
| `result.json` v1, validity and SLO evaluation (ADR-004) | `internal/result` | Every emitted document validated against the embedded schema in tests |
| Aligned terminal tables | `internal/render/cli` | 91% coverage, golden files, determinism and ANSI-alignment tests |
| Engine orchestration | `internal/engine` | 81% coverage; one monotonic start shared by every runner |
| `run`, `validate`, `schema`, `version`; `--output json`; exit codes | `internal/cli` | End-to-end tests against `httptest` for every exit code |
| Schemas embedded in the binary | `internal/schemas` | `tracepoint schema <name>` emits exactly the contract the binary enforces |

### Evidence

```
$ make check                 # exit 0: fmt, vet, lint (0 issues), race tests, contracts, govulncheck
$ make cover                 # total: 81.6% of statements
  internal/schedule 95.4%  internal/metrics 93.0%   (both above the 85% gate)
$ go test ./internal/metrics/ -bench=BenchmarkRecord -benchmem
  BenchmarkRecord-14           0 B/op   0 allocs/op   51 ns/op
  BenchmarkRecordParallel-14   0 B/op   0 allocs/op  173 ns/op
```

A real run against a local server, which is what the end-to-end suite automates:

- weighted 4:1 traffic produced a 186:43 split;
- the loopback target was detected and the run marked `degraded` with the same-host caveat;
- a p99 of 376ms against a 250ms budget produced `SLO FAIL` and exit 1;
- the emitted `result.json` validated against the embedded result schema with **zero** violations.

### Bugs found by the tests, and fixed

Three of these were behavioural bugs that would have shipped:

| Found by | Bug | Fix |
| --- | --- | --- |
| End-to-end JSON test | On an SLO breach, stdout carried **two** JSON documents - the result and then an error envelope - breaking the §6.1 contract that it carries exactly one | An outcome already described in the emitted document is marked `documented`; the exit code alone conveys it |
| goleak | The second-interrupt watcher never exited, leaking a goroutine on every run | It now selects on a `finished` channel and returns with the command |
| Renderer colour test | `%-9s` padded strings that already contained ANSI escapes, so the label column collapsed whenever colour was on - visible only on a real terminal | Padding is applied to the visible text and colour added afterwards |
| Executor context test | Cutting off in-flight work left arrivals being generated and immediately cancelled, inflating the offered count with load the target never saw | Arrival generation watches both contexts |
| Schema fixture round-trip | The schema let a runner omit its executor, though "how much load" is the one thing a user must state | The schema now requires an executor naming a rate, a vu count or a stage profile |
| Contract script | iCloud had silently duplicated seven fixture files as `name 2.json`, which globs picked up | `make check` now fails on duplicate copies |
| govulncheck | `go 1.26.1` carried 13 standard-library CVEs reachable from this code | Pinned `go 1.26.6`, the first release with no findings |

### Deviations from the spec

| Deviation | Reason |
| --- | --- |
| `internal/engine` and `internal/errs` are not in the §3.1 layout | The run has to be orchestrated somewhere, and coded errors have to sit below every package that constructs one. Both are small and neither holds policy |
| The JSON Schemas live in `internal/schemas/` rather than at the repository root | `go:embed` cannot reach outside its package, and embedding is what guarantees `tracepoint schema` emits the contract the binary actually enforces |
| Analysis is validity and SLO only; the verdict says so | Hot buckets, incidents, culprit ranking and correlation are phase 3, and they need the telemetry samplers that corroborate them. The verdict reports what can honestly be concluded rather than guessing |
| Journeys, templates and the `vus` executor are refused with a coded error | They are phases 2 and 6. Refusing is better than silently ignoring: §4 forbids ever sending a literal `{{token}}` |
| SQL and Redis sections are rejected by the engine | Phase 2. The error names the runners this build has |

### Risks carried into phase 2

| Risk | Mitigation |
| --- | --- |
| Per-bucket memory has not been measured over a long run | The soak target exists; it runs in phase 8 when there is enough to soak. Sealing is unit-tested and the sketch budget is measured in ADR-002 |
| `serviceTimeRose` distinguishes a saturated target from a capped client with a crude first-third/last-third comparison | It is the cautious reading - flat means invalid - and phase 3 replaces it with the rolling-median machinery the incident rules already need |
| Coverage counts the end-to-end tests via `-coverpkg`, so a package can look covered without its own tests | The math packages carry their own gates, and phase 3 adds unit tests to `result` alongside the analysis it gains |

## Phase 0 — complete

### Delivered

| Deliverable | Where | Evidence |
| --- | --- | --- |
| Spec stored verbatim (§0.1) | `docs/SPEC.md` | 362 lines, byte-for-byte as supplied |
| Product restated in ten lines (§12.0) | `docs/ARCHITECTURE.md` | Top section |
| Assumptions and open questions (§12.0) | `docs/ASSUMPTIONS.md` | 12 assumptions, 7 open questions, each with a default already applied |
| Architecture | `docs/ARCHITECTURE.md` | Flow, package map, three extension points, concurrency, v2 note |
| ADRs 001–009 plus 010 | `docs/adr/` | 10 records, indexed in `docs/adr/README.md` |
| JSON Schemas (config, policy, result, digest, events, error) | `internal/schemas/*.json` | All six pass Draft 2020-12 meta-validation |
| Schema fixtures | `internal/schemas/testdata/config/{valid,invalid}/` | 9 valid, 16 invalid, each verified to land on the side its directory claims |
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
