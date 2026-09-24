# Progress

> Read [`docs/SPEC.md`](SPEC.md) first — it is the source of truth. This file records
> what is actually built, what deviates, and what is known to be missing. It is updated
> at the end of every phase, before the commits for that phase.

## Status

| Phase | Scope | State |
| --- | --- | --- |
| 0 | Plan: spec stored, ADRs, JSON Schemas, docs, skeleton, Makefile, lint, CI stub | Complete, approved |
| 1 | Engine slice: clock, schedule, sketches, recorder, arrival-rate executor, HTTP runner, `result.json` v1, CLI table, JSON output, exit codes | Complete, approved |
| 2 | Storage and safety: SQL and Redis runners, templates, `--set`, policy, guards, redaction, preflight, `doctor`, `--dry-run` | Complete, approved |
| 3 | Analysis: telemetry samplers, incidents, culprits, correlation, verdicts, validity, digest; faultbox and the known-answer suite | Complete, approved |
| 4 | Agent interfaces: run store, detach, events, audit log, ops registry, `capabilities`, MCP, REST, parity tests | Complete, approved |
| 5 | Human interfaces: HTML, Markdown, JUnit, `report`, TUI | **Complete — awaiting approval** |
| 6 | Journeys and onboarding: vus executor, extract/expect, feeders, think time, cookies, strain finder, `quick`, `init` | Not started |
| 7 | Capacity and compare: auto-ramp, USL, confidence intervals, gates, incident diff | Not started |
| 8 | Integration and release: Go facade, `tracepointtest`, Action, Docker, skill, contract CI, fuzzing, soak, GoReleaser, agent dogfood | Not started |

## Phase 5 — complete

A person now gets what an agent already had: every run writes a single-file HTML
report that opens offline, a Markdown summary and JUnit XML are one command away, and
a terminal shows the run live. Every one of them is a pure function of `result.json`,
so `tracepoint report` reproduces what a run wrote, byte for byte.

### Delivered

| Deliverable | Where | Evidence |
| --- | --- | --- |
| HTML report: one file, uPlot 1.6.32 vendored with `go:embed` (licence and provenance beside it, and inside the report), display data in a JSON script block, CSP `default-src 'none'` with script and style hashes computed from the bytes written | `internal/render/html` | `TestCSPCoversExactlyTheInlineContent`: the policy's hashes equal the inline content's, no inline handlers or style attributes; `TestVendoredAssetsArePinned` |
| Sections: validity and SLO badges, invalid-run warning, verdict with evidence and next steps, validity findings and SLO checks, incidents with zoom-to-timeline, shared timeline, per-runner and per-label tables, error classes, status histogram, HTTP phases, pool detail, correlation, telemetry panels with statements, capacity (levels, boundary, USL, chart), methodology and thresholds, files, export, third-party notices | `assets/report.tmpl` | Screenshots reviewed in light and dark; every section present in `TestInABrowser`'s fixture |
| Timeline: p50/p95/p99 and response/service toggles, in-flight on a right axis, log scale, reset zoom; warm-up, ramps and incidents shaded; thin buckets as hollow points apart from the line; the timeline as a table for keyboard and screen-reader users | `assets/report.js`, `view.go` | `TestBandsAndGaps` |
| Max-preserving thinning to at most 2,000 points per series, display only | `downsample.go` | Written test-first; a rapid property test (at most the limit, x increasing, the global maximum exact); `TestLongRunsAreThinnedForDisplayOnly` (a one-bucket spike of 777ms survives 5,000 buckets) |
| Above 5 MB, display data only, and the report says where `result.json` is | `html.go` | `TestLargeResultEmbedsDisplayDataOnly` |
| Offline: nothing that can fetch | | `TestNoExternalReferences`: no loading tags, no attribute pointing outside the page, no CSS `url()`/`@import`, no network APIs in any script |
| The XSS string renders inert | | `TestHostileStringsRenderInert` (escaped as text, script elements balanced, JSON blocks intact); `TestInABrowser` (no `<img>` in the live DOM) |
| A real browser runs it | | `TestInABrowser`: headless Chromium loads the report; every script runs under the policy, the four charts draw, and the console is silent - so no request was even attempted, since `default-src 'none'` logs any it refuses. Shown to catch a wrong hash |
| Colourblind-safe (Okabe-Ito) palette, dark mode, focus rings, skip link, captions and scoped headers, print styles | `assets/report.css` | Reviewed in both themes |
| Markdown for PR comments and job summaries | `internal/render/markdown` | Golden files; `TestHostileTextIsLiteral` (no markup, links, HTML or @mentions survive) |
| JUnit XML whose failures mirror the exit codes | `internal/render/junit` | Golden file; `TestOutcomesMirrorExitCodes`; `TestHostileTextStaysWellFormed` |
| `tracepoint report <run>` (html, markdown, junit; `--out`; `--output json`) | `internal/cli/report.go` | `TestRunWritesItsDirectory`: the command re-renders `report.html` byte for byte; markdown to stdout; junit with `--output json`; refusals |
| `render_report` operation (markdown or junit inline, html by path) | `internal/ops/report.go` | `TestAgentLoop`; the parity test; MCP and OpenAPI golden files |
| Every run writes `report.html`; `--report-path` | `internal/session` | `TestRunWritesItsDirectory`; the secret scan now reads every file in the run directory |
| Live view at a terminal; one progress line every 5s elsewhere; `--no-live` | `internal/live`, `internal/cli/progress.go` | `internal/live` model and driver tests; `TestLiveViewAtATerminal`; `TestPlainProgress`; run by hand on a real pseudo-terminal |
| Result schema 1.1: `executor.start_target` | `internal/result`, schema | `TestRampRecordsItsStart`; additive, so a minor bump |

### Evidence

```
$ make check
  fmt, vet, golangci-lint (0 issues), race tests, contract checks: all pass
  govulncheck: could not run - vuln.go.dev is not reachable from this environment (CI runs it)
$ make integration                 # exit 0
$ make e2e                         # FAILS in this environment - see "The clean run and vCPU stalls" below
  TestKnownAnswers/clean_run_has_no_incidents fails in 3 runs of 5 at this commit
  - and in 4 of 5 at the phase 3 commit (5ecc6d6), run back to back on the same host
coverage: render 100%  render/cli 92.6%  render/html 91.7%  render/markdown 92.2%
          render/junit 88.7%  render/report 89.5%  live 89.0%
```

### Bugs found, and fixed

| Found by | Bug | Fix |
| --- | --- | --- |
| Watching the live view on a real terminal | Progress reported nothing done until a short run ended: `Done` came from sealed buckets, which trail the run by the operation timeout (10s by default) | `Done` is dispatched minus in flight; errors and p99 stay sealed-only, and the view says they trail |
| Writing the `--report-path` test | A detached run ignored `--result-path`: the hand-off to the child never carried it | Both copies travel in the hand-off as absolute paths; the test fails without them |
| The browser test | A Linux locale of C or POSIX surfaces as `en-US@posix`, which `Intl` rejects, so uPlot threw at load and no chart drew | A preamble falls back to `en-US` only when `Intl` rejects the browser's tag |
| The race detector | The "starting" log line reached stderr after the live view had started drawing there | The view takes the log over the moment it starts |
| Self-review against ADR-002 | Thin buckets were gaps; ADR-002 says they are drawn, but never trusted | A points-only series per runner, apart from the line |

### The clean run and vCPU stalls - a decision needed

The known-answer suite passed twice at the end of phase 3. On this host it now fails
most runs of the clean scenario, **at the phase 3 commit as well as this one**, so it
is not a regression from phases 4 or 5. The cause, from the failing runs' own
generator telemetry: in one bucket, every runner's dispatch lag jumps together from
about 1ms to 14-24ms while GC pauses stay under 0.4ms, Go scheduler latency under
0.4ms and CPU near 5%. That is the whole process - once, the target too - being
paused, not the target slowing. The host is a Firecracker microVM, and `/proc/stat`
shows steal time: 0.54s stolen from the vCPUs during a single 45s run.

TracePoint reports it faithfully. The stall is 3x and over 5ms above the baseline, so
the spec's relative rule makes the bucket hot. Client wait dominates, so the incident
is `client_limited` and the verdict blames the generator, which is true. But the
spec also says a clean run has zero incidents. One failing run also showed a real
weakness: a one-bucket Redis blip during the handler-delay fault joined the app's
incident and was named culprit, although it went hot four buckets after the
application at 0.2x its threshold (score -6).

Options, none taken without approval:

1. **Detect generator stalls and keep them out of incidents.** When dispatch lag
   rises on every runner in the same bucket, mark the bucket stalled: it cannot be
   hot, and a `GENERATOR_STALL` finding degrades validity instead. Keeps the spec's
   intent (a clean target yields no incidents, and the run says it was disturbed);
   adds a rule and a code. My recommendation.
2. **Require two buckets for a relative-only hot streak.** Simple; stops one-bucket
   blips everywhere, but also delays detecting a real short spike by a bucket, and
   departs from §5.5 as written.
3. **Accept a one-bucket `client_limited` incident in the clean scenario.** Changes
   only the test; the product then reports every host hiccup as an incident.
4. **Run the suite only on dedicated hosts.** Changes nothing; CI on shared runners
   would stay flaky.

Separately, whatever is chosen: a culprit that went hot after the application and
below its threshold should not be named (a negative score means the evidence points
away from it); the incident should be reported without a culprit. That is a small,
test-first change to `internal/analysis`, also awaiting approval.

Fixed now, because it is plainly a bug: since phase 4 every run writes a directory,
and the suite wrote them into `tools/faultbox/runs/` in the source tree. It now uses
the scenario's temporary directory. A failing scenario also logs the generator's
telemetry around each incident, which is how this cause was found.

### Deviations from the spec, and judgements it left open

| Item | Reason |
| --- | --- |
| uPlot is the chart library | The spec says "vendored" without naming one. ~50 KB, time-series native, canvas, CSP-compatible, MIT (ADR-007) |
| The CSP also sets `base-uri 'none'` and `form-action 'none'` | Stricter than the spec's minimum; the report needs neither |
| Charts need JavaScript; every figure they show is also in a table | The spec's sections are served without script; the charts cannot be |
| The timeline shows one quantile at a time, chosen by a toggle | Three quantiles for three runners is nine lines on one chart |
| Markdown escapes aggressively (`\.`, `\:` and so on), and puts a zero-width space after `@` | The raw text is noisier, but it renders clean, and a hostile target cannot format, autolink or mention anyone in a pull request |
| JUnit: incidents and the verdict are context in `system-out`, not test cases | They do not fail a run by themselves; the test cases mirror the exit codes |
| The live view's errors, rps and p99 trail by the operation timeout | A bucket seals only when no operation that belongs to it can still complete, which is what makes its figures final; showing unsealed figures would show numbers that later change. The view says so |
| The live view takes no keyboard input | So ctrl+c stays a signal with its existing meaning, graceful then immediate, rather than a key event the view would have to reinterpret |
| `render_report` returns html by path only | The report is for a person; putting 200 KB of HTML into a model's context helps no one |
| Result schema 1.0 → 1.1 | `start_target`, needed to tell a ramp from a hold; additive |

### Risks carried into phase 6

| Risk | Mitigation |
| --- | --- |
| The browser test skips where no Chromium is installed | The structural tests hold everywhere; CI installs Chromium for it (phase 8) |
| uPlot is one maintainer's library | It is vendored and pinned; the report does not depend on its future |
| Bubble Tea v1 while v2 exists | v1 is stable and does what is needed; the view is small, and its model is independent of the terminal library's driver |
| Charts are drawn on canvas, which screen readers cannot read | Every chart has a label and a table equivalent |
| `make e2e` is red on this host | Diagnosed above; awaiting a decision between the options |

## Phase 4 — complete

An agent can now drive TracePoint end to end without a terminal: over MCP, over REST
or from a shell, it can read the policy, scaffold and validate a configuration, plan
it, start a run that outlives the call, poll it within a tool-call deadline, stop it,
and read its digest or any section of its result. All three surfaces come from one
registry, and a parity test holds them together.

### Delivered

| Deliverable | Where | Evidence |
| --- | --- | --- |
| Run store: `runs/<UTC timestamp>-<id>/` with `state.json` (atomic), `events.ndjson`, `result.json`, `digest.json`, `config.effective.yaml`, `run.log`; terminal status is final; heartbeat every 2s, `lost` after 15s stale with a dead pid; `STOP` sentinel; GC that never removes a live run; `audit.ndjson` | `internal/runstore` | `runstore_test.go`: lost detection on a fake clock, the sentinel, `Wait`, GC, id/prefix/suffix/path resolution; `TestCreateLimitedIsAtomic` |
| The concurrent-run limit checked and the run created under one lock (process mutex and a `flock` on the run root) | `runstore.CreateLimited` | `TestCreateLimitedIsAtomic`: 16 concurrent creators across two stores on one root, exactly one wins; the test fails with the locks removed |
| NDJSON events: `run.started`, `preflight.completed`, `bucket.sealed`, `incident.opened/closed`, `warning`, `run.finished` with the digest embedded; sequence-numbered, on the run clock | `internal/events`, `internal/engine/events.go`, `internal/session` | Every event validated against `events.schema.json`; a failing sink never stops a run; `TestEventsOnStdout` |
| One run path for every caller: load, prepare, preflight, execute or detach, finish, audit | `internal/session` | Exercised by the CLI, ops and adapter suites (76% of its functions' statements across them) |
| `run --detach` returns only after validation and preflight pass; the child gets its hand-off (including resolved secrets) on stdin, never on disk | `session/detach.go`, hidden `__run-detached` | `TestDetachReportsPreflightSynchronously`, `TestDetachWaitStop` (real binary, real child process) |
| `status`, `wait` (exit 5 on timeout, the run's own code when it ends, 3 if lost), `stop` (`--force` kills), `list`, `gc`; `digest` accepts run ids | `internal/cli/runs.go` | `TestListStatusAndGC`, `TestDetachWaitStop` |
| `config.effective.yaml` keeps `${ENV}` references, redacts inline secrets; `--from <run>` re-runs it and refuses until a redacted secret is re-supplied with `--set` | `internal/config/effective.go` | `TestEffectiveYAMLKeepsReferencesAndRedactsInlineSecrets`, `TestFromNeedsRedactedSecretsAgain`; no secret appears in any artifact |
| `--set` on map keys (`http.headers.Authorization=...`) | `internal/config/set.go` | `TestSetAddressesMapKeys` |
| Operation registry: 11 operations with model-oriented descriptions, annotations, schemas derived from handler types, strict validated input; the server's policy fixed at startup; `config_path` confined to the server directory, symlinks included | `internal/ops` | `ops_test.go`: `TestInputValidation`, `TestPolicyIsTheServers`, `TestPolicyCannotBeExceeded`, `TestConfigSources`, `TestAgentLoop`, `TestStopRun` |
| MCP server (official Go SDK): tools with structured content and a text summary, `isError` results carrying the envelope; schema and docs resources; digest and result templates; three prompts; stdio or streamable HTTP | `internal/adapters/mcp`, `tracepoint mcp` | `mcp_test.go` over the SDK's in-memory transport; `tools.golden.json`; `TestAgentLoopThroughMCPStdio` against the real binary |
| REST server: `POST /v1/ops/<name>`, generated OpenAPI 3.1, health check; statuses by error class | `internal/adapters/rest`, `tracepoint serve` | `rest_test.go`, `openapi.golden.json`; `TestAgentLoopThroughServe` against the real binary with detached runs |
| HTTP guard: bearer token (constant-time), Host allowlist with loopback aliases, Origin refused unless allowed, preflights refused, no CORS headers ever, loopback bind unless `--allow-remote`, generated token in a 0600 file | `internal/adapters/guard` | `TestGuard` (each check alone, and that no response carries `Access-Control-*`), `TestGuardConstruction` |
| `capabilities`: generated from the cobra tree and the registries | `internal/cli/agents.go` | `TestCapabilities` |
| Parity: registry = MCP tools = REST paths = `capabilities.operations`; every operation's CLI equivalent names a real command and real flags | `internal/cli/parity_test.go` | `TestParity` |
| `init` (the same scaffold as `scaffold_config`) | `internal/cli/agents.go` | `TestInit`, `TestScaffoldValidates` |

### Evidence

```
$ make check
  fmt, vet, golangci-lint (0 issues), race tests, contract checks: all pass
  govulncheck: could not run - vuln.go.dev is not reachable from this environment (CI runs it)
$ GOOS=windows go build ./... && GOOS=darwin go build ./...     # both build
coverage, own package:   events 95.5%  plan 89.4%  mcp 86.7%  rest 84.0%  config 80.7%
                         runstore 78.2%  ops 77.9%
coverage, across suites: guard 88%  runstore 84%  session 77%  (mean of function coverage;
                         both are exercised mainly from the CLI, ops and REST suites)
```

Checked by hand with the real binary: detach, status, list, wait while running (exit 5),
stop (interrupted, exit 3, partial result written), `--from` refusing a redacted secret
and succeeding with `--set`, `--events -` producing sequence 1..n ending in
`run.finished` with the digest, `gc`, and no secret in any file under the run root.

### Bugs found, and fixed

| Found by | Bug | Fix |
| --- | --- | --- |
| Self-review | Two `start_run` calls arriving together (REST handlers and MCP tool calls run in parallel) could both count the active runs before either created one, and both start under a policy allowing one | `CreateLimited` counts and creates under one lock; a regression test that fails without it |
| The race detector | The progress snapshot was read by the heartbeat while the sweeper wrote it | Guarded by a mutex |
| Writing `config.effective.yaml` | A header comment containing `${ENV}` was interpolated, because interpolation runs on the raw text, comments included | Reworded; interpolating comments stays, as the spec's "any part of the document" |
| The `--from` test | A redacted password inside a DSN is URL-escaped, so the redaction marker was not recognised and `--from` did not ask for the secret again | Both spellings are recognised |
| `capabilities` | `init --out` claimed the shorthand `-o`, already the global `--output`; cobra panics merging them | No shorthand on `--out` |

### Deviations from the spec, and judgements it left open

| Item | Reason |
| --- | --- |
| Adapters reach the core through `internal/session` and `internal/ops`, not the public Go facade | The facade is phase 8 (§12). The operations are written against `session`, which is the facade's intended body, so the phase 8 change is a move rather than a rewrite |
| `compare_runs` is not in the registry yet | `compare` is phase 7; the operation arrives with it, and the parity test will hold it then |
| `incident.opened` and `incident.closed` are emitted when the run ends, not live | Incidents are a whole-run analysis (§5.5 merges and classifies over the full timeline). A live, provisional incident stream would disagree with the final result |
| `level.started` and `level.completed` are declared but not emitted | They belong to auto-ramp, phase 7 |
| `report.html` is listed in the run's artifacts but not written | The HTML report is phase 5 |
| An out-of-bounds `config_path` is `OPS_INVALID_INPUT` (HTTP 400), not a policy refusal | It is malformed input, not something a human could grant |
| `init` arrives in phase 4 rather than 6 | `scaffold_config` needed the scaffold; the command costs nothing more. Phase 6 adds project detection to it |
| The server policy defaults to `ServerDefault()`: 500 req/s per runner, 512 in flight, 10m, one run at a time, no writes, private targets | Tighter than the CLI default because the caller is a program and nobody is necessarily watching (§6.5) |
| The token lives in `<run-root>/.<serve|mcp>-token` unless given | It must survive restarts so a configured client keeps working, and must not be printed where logs are collected |
| `wait_for_run` is capped at 50s per call | Below common tool-call deadlines; the agent polls in a loop it controls (ADR-008) |

### Risks carried into phase 5

| Risk | Mitigation |
| --- | --- |
| On non-unix platforms the concurrent-run lock is per process, and liveness cannot be checked, so a run is `lost` on a stale heartbeat alone | Both are documented at the code. Windows builds; its detach path has not been run |
| The MCP specification and SDK are still moving | The SDK is pinned; the tool list is a golden file, so any change in what an agent sees is a reviewed diff |
| `session` has little coverage from its own package | Its paths are covered by the CLI, ops and adapter suites, including real detached child processes |
| `govulncheck` still cannot reach vuln.go.dev from here | CI runs it; this phase added the MCP SDK and its indirect modules, all recorded in ADR-007 |

## Phase 3 — complete

The tool now answers the question it exists for. Every verdict class is proved end to
end: faults with known causes are injected into a real application over real Postgres
and Redis at known times, and TracePoint names each one - class, culprit, window
within one bucket, verdict and exit code. The rules and every constant are in
[`METHODOLOGY.md`](METHODOLOGY.md).

### Delivered

| Deliverable | Where | Evidence |
| --- | --- | --- |
| Hot buckets, incidents, culprit ranking, lagged Spearman, templated verdicts, confidence rubric (ADR-005) | `internal/analysis` | Written test-first; 92.2% coverage; rapid properties for Spearman (bounded, symmetric, invariant under monotonic transforms), the median, and episode merging (idempotent, covering, non-overlapping); every scenario's output validated against the result schema |
| Validity: `TELEMETRY_UNAVAILABLE`, `LITTLES_LAW_INCONSISTENT`, saturation judged against steady buckets | `internal/analysis/validity.go` | `validity_test.go` |
| Telemetry: sampler registry and loop; generator, Postgres, MySQL and Redis samplers | `internal/telemetry` | Unit tests for the loop's isolation and fail-soft behaviour; integration tests against each real server, including a waiting lock seen by both lock signals and a role without `pg_read_all_stats` |
| Digest with a character budget and machine-applicable recommendations; `tracepoint digest` | `internal/result/digest.go`, `recommend.go`, `internal/cli/digest.go` | Golden file; every digest schema-validated; the budget holds for the bytes actually written; the sentinel secret never appears in a digest |
| Incident table and tracking line in the terminal summary | `internal/render/cli` | `incidents.golden` |
| `--http-threshold`, `--db-threshold`, `--redis-threshold` | `internal/cli/commands.go` | |
| faultbox | `tools/faultbox` | Faults scheduled from the run's first load request; records when each actually held |
| Known-answer suite (`make e2e`) | `tools/faultbox/knownanswer_e2e_test.go` | Six scenarios, below |
| Integration suite (`make integration`) | `internal/telemetry`, `internal/cli` | Three-tier runs against Postgres and MySQL with every sampler on |

### The known answers

Each assertion is against faultbox's own record of when the fault held. All six pass
under `-race`, in three consecutive full runs of the suite (plus the scenario-by-scenario runs while building it).

| Fault | Required and observed |
| --- | --- |
| none | zero incidents, verdict `none`, exit 0 |
| `LOCK TABLE items` for 5s at 20s | `correlated` over buckets 20-25, culprit `db` alone, postgres `locks_waiting` 0 -> 301, exit 1 |
| `LOCK TABLE probe_only` for 5s | `storage_only` over 20-24, exit 0 |
| 400ms handler delay for 5s | `app_only` over 20-25, verdict `app`, exit 1 |
| Redis `DEBUG SLEEP 3` | `correlated` over 20-23, culprit `redis`, corroborated by the sampler's own INFO stalling (`sample_ms`), exit 1 |
| 300ms endpoint, `max_in_flight: 2` | `CLIENT_CAPPED`, invalid, `client_limited` incident, verdict `client`, exit 4, no incident blames the target |

### Evidence

```
$ make check
  fmt, vet, golangci-lint (0 issues), race tests, contract checks: all pass
  govulncheck: could not run - vuln.go.dev is not reachable from this environment (CI runs it)
$ make integration           # exit 0
$ make e2e                   # exit 0, twice in a row: tools/faultbox 257s, 257s
$ make cover                 # total: 79.4% of statements
  analysis 92.2%  schedule 92.5%  metrics 92.4%   (the 85% gate)
  telemetry 53% in the unit run; its Postgres and MySQL paths are covered by make integration
```

### Bugs found, and fixed

| Found by | Bug | Fix |
| --- | --- | --- |
| The known-answer suite under `-race` | The SQL and Redis runners incremented their read and write tallies from every worker with no synchronisation. Unit tests drove `Do` sequentially and never saw it | Atomic counters, and a concurrent regression test per runner that reproduces the race on the old code |
| The same | faultbox encoded a fault in its reply while the goroutine applying it was already mutating it | Reply with a copy taken under the lock |
| The first real run | Redis throughput fell during a *database* lock, because the application stopped reaching the cache, and counted as corroboration for Redis | Throughput is no longer a signal: it cannot tell cause from victim |
| The first real run | HTTP templates were refused with a message pointing at phase 2; the phase-2 template engine had never been wired into the HTTP runner | Templates compile at load and render per request |
| Wiring those templates | A templated url would have been missing from `Targets()`, so its host would have escaped the policy check | Hosts are fixed at load; a template in a scheme or host is refused |
| The uniform-breach test | A strong whole-run correlation alone named a storage tier as the bottleneck | It is reported but never promoted to an answer on its own (ADR-005 §4) |
| Flaky runner tests (half of runs) | The harnesses scheduled iterations at fixed offsets, so fast local operations completed before they were due and their response time clamped to zero | Each iteration is due when dispatched |

### Deviations from the spec, and judgements it left open

| Item | Reason |
| --- | --- |
| "±1 bucket tolerance" read literally: two runners' episodes join when shifting one by a bucket makes them overlap, so adjacent episodes join and a one-bucket gap does not | Within a runner, gaps of one bucket are bridged, as §5.5 says separately |
| "client_wait dominates" means client wait is at least half the response p99, at the median over the runner's hot buckets, and an incident is `client_limited` only when every hot runner is dominated | The spec gives no number. Requiring every hot runner keeps a database stall that also backs up our queue a database finding |
| A runner is sufficient over an incident when at least half its buckets there are eligible | The spec requires sufficient data without defining it over a window |
| Culprit score `2·lead + log2(severity) + corroboration`, ties within 0.25 | The spec orders the signals but gives no weights |
| A degraded run caps confidence at medium, where phase 1 forced it to low | Every loopback run is degraded; forcing low would make every local verdict a non-finding |
| `tracepoint digest` takes a result path or run directory; run ids are resolved in phase 4 | The run store is phase 4 |
| The client-limited scenario uses 5s buckets | Two workers against a 300ms endpoint complete ~6 operations a second, so every 1s bucket would be insufficient. Wider buckets keep the evidence bar; lowering `min_samples` would not |
| A/A and +50ms compare scenarios, and the MCP/REST flow | Phases 7 and 4, which build what they test |
| Strain finder | Phase 6 per §12 |
| Preflight requests carry `X-Tracepoint-Preflight: 1` | Additive. Lets a target leave them out of its metrics, and lets faultbox start its clock at the first real request |

### Risks carried into phase 4

| Risk | Mitigation |
| --- | --- |
| Every run on one machine is degraded by the same-host caveat, which caps confidence at medium | Correct, and the reason is stated. Phase 8 dogfooding and the Compose demo will say how to run the generator elsewhere |
| The relative rule could flag scheduling noise on a heavily loaded CI host as an incident in a clean run | Its three guards; the clean scenario passed every run here; nightly CI runs the suite five times |
| Signal thresholds were calibrated on faultbox | They are one table in `signals.go`, documented, and only corroborate - no signal alone makes a verdict |
| Postgres 15+ flushes statistics about once a second, so counter deltas can lag a bucket | The lock and session signals are read live from `pg_stat_activity` and `pg_locks`; the window is padded by a bucket |
| `govulncheck` could not reach vuln.go.dev from this environment | CI runs it; nothing in this phase changed a direct dependency except adding testcontainers-go, which is test-only |

## Phase 2 — complete

All three tiers now run on one clock, and everything that can be refused is refused
before a single operation is performed.

### Delivered

| Deliverable | Where | Evidence |
| --- | --- | --- |
| Template mini-language, compiled once at load | `internal/template` | 93% coverage, 15.6M fuzz executions with no panic; render 86ns, static 2ns with no allocation |
| Policy envelope and tighten-only merge | `internal/policy` | 90% coverage; a configuration asking for more than the policy grants is refused, not clamped |
| SQL and Redis statement classifiers | `internal/policy/classify.go` | 6.9M and 6.2M fuzz executions; both fail closed, and a keyword inside a string literal is data rather than a statement |
| SQL runner | `internal/runner/sqlrun` | 77% coverage against real SQLite; connections acquired with `db.Conn` so pool wait is timed apart from query time; every read iterated and closed |
| Redis runner | `internal/runner/redisrun` | 82% coverage against miniredis; pipelines timed as one unit; a cache miss is an answer, not an error |
| `--set` overrides | `internal/config/set.go` | List items addressable by name; an unknown path is an error with a suggestion, never a silent no-op |
| Redaction | `internal/config/redact.go` | A sentinel secret appears in no artifact, while the host and database name survive |
| Abort guard | `internal/engine/guard.go` | Trailing window; 4xx and 429 deliberately excluded |
| Preflight with policy checks, `doctor`, `--dry-run` | `internal/cli` | Targets resolved and judged before anything connects |

### Evidence

```
$ make check                 # exit 0
$ make cover                 # total: 82.1% of statements
  schedule 95.4%  metrics 93.0%  template 93.0%  policy 90.4%  config 85.2%
```

A three-tier run against a local HTTP server, SQLite and Redis, on one clock:

```
  RUNNER  KIND     N    RPS   ERR    p50    p95    p99    max
  http    app      121  24.2  0.00%  17ms   26ms   27ms   27ms
  db      storage  161  32.2  0.00%  1.3ms  1.6ms  1.8ms  2.1ms
  redis   storage  241  48.2  0.00%  1.3ms  1.6ms  1.8ms  2.2ms
```

### Bugs found by the tests, and fixed

| Found by | Bug | Fix |
| --- | --- | --- |
| A real three-tier run | The engine set the monotonic origin on the HTTP runner only, so the SQL and Redis runners measured from the zero time and reported latencies of **9.2e12 ms** - several thousand years. Every count and error rate looked perfect | `SetStart` moved into the `Runner` interface, so the compiler requires it of every implementation. An end-to-end assertion now rejects implausible latencies, which no assertion on counts would have caught |
| The same run | The loopback caveat was reported once per runner, so one fact read as three problems | Validity findings are deduplicated |
| `--set` end-to-end test | Overriding `run.duration` left the `rate:` shorthand's derived stage at the old length, and the configuration then contradicted itself | Overrides apply to the document as written, before defaults are derived - which is what "flags above file" in the precedence chain actually means |
| `govulncheck` | `golang.org/x/text` arrived with the drivers carrying a known CVE | Upgraded to v0.39.0 |

### Deviations from the spec

| Deviation | Reason |
| --- | --- |
| `{{var}}` and `{{feeder.column}}` compile but cannot resolve yet | They are filled by journey extraction and CSV feeders, both phase 6. A template that cannot resolve fails the operation with `extract_failed` rather than sending a literal token, which is what §4 requires |
| Bind arguments gained a `{value, secret: true}` object form | §8 requires args marked `secret: true`, and the schema had no place for it. Additive |
| Redis service time is marked at hand-off to the client, not at connection acquisition | go-redis owns its pool and does not expose the moment a connection is in hand. Pool waits are reported through `PoolStats` rather than being silently folded into service time. The SQL runner, which can acquire explicitly, does |

### Risks carried into phase 3

| Risk | Mitigation |
| --- | --- |
| The SQL and Redis runners are tested against SQLite and miniredis, not Postgres, MySQL or a real Redis | The testcontainers integration suite covers those; it is written in phase 3 alongside the telemetry samplers that need the same containers |
| `serviceTimeRose` still uses the crude first-third/last-third comparison | Phase 3 replaces it with the rolling-median machinery the incident rules need anyway |
| Statement classification is a heuristic and will mislabel something eventually | It fails closed, the declared `type` overrides it, and a declared read that looks like a write is reported as a warning rather than silently trusted |

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
