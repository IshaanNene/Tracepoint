# Changelog

Notable changes to TracePoint. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Contracts under semantic versioning are listed in §6.7 of
[`docs/SPEC.md`](docs/SPEC.md): CLI commands and flags, exit codes, error codes, the
JSON Schemas, the agent operations exposed over MCP and REST, and the public Go API.
Within a major version, changes to them are additive only. A deprecation appears here,
in `capabilities.deprecations` and as a warning event for at least one minor version
before it is removed in the next major.

**Pre-1.0, these contracts may change between minor versions.**

## [Unreleased]

### Added

- Phase 6: journeys and onboarding. Multi-step HTTP journeys with `extract` (gjson
  paths), `expect` (status sets, JSON-path equals and exists, body size), per-step
  timeouts, think time, cookie jars and CSV feeders, checked at load time so that no
  step reads a variable nothing extracted. `http.traceparent` sends W3C trace context.
  The closed-model `vus` executor with `pick: per-iteration` or `per-vu`. The strain
  finder: on a ramping run, "strain begins at ~N users (~R req/s)" and the next
  capacity window, in the result, the digest and every report. `tracepoint quick
  <url>`, a zero-configuration smoke test that probes a database and Redis on the same
  clock. `init --detect <dir>` infers a starter configuration from compose files,
  environment variable names and OpenAPI documents, with evidence and confidence for
  each inference; `init --from-openapi` imports safe requests from an OpenAPI or
  Swagger document. `scaffold_config` accepts `detect_dir` and `openapi_path`. The
  digest's `caveats` include `PROBE_TRIVIAL` for a `SELECT 1` probe.
- Phase 5: human interfaces. Every run writes `report.html`: one file that opens
  offline, with a strict Content-Security-Policy, the verdict, incidents, a shared
  timeline with quantile and response/service toggles and shaded warm-up, ramps and
  incidents, per-runner tables, telemetry and capacity panels, dark mode and a
  colourblind-safe palette. `tracepoint report` renders any run as HTML, Markdown (for
  pull requests and job summaries) or JUnit XML; `render_report` does the same over
  MCP and REST; `--report-path` keeps a copy. At a terminal a live view shows progress,
  per-runner throughput, sealed p99 and its trend; elsewhere one progress line every
  five seconds, and `--no-live` opts out.
- Phase 4: agent interfaces. Every run gets a directory in the run store with its
  state, events, result, digest, effective configuration and log; `run --detach`
  returns once preflight passes, and `status`, `wait`, `stop`, `list` and `gc` manage
  runs. `--events -` streams NDJSON ending in `run.finished` with the digest. `--from
  <run>` re-runs a run's effective configuration, which keeps `${ENV}` references and
  redacts inline secrets. `--set` addresses map keys. An audit log records every
  start and stop with its actor. Eleven agent operations in one registry, served as
  MCP tools (`tracepoint mcp`, stdio or streamable HTTP) with schema, documentation
  and run resources and three prompts, and as a REST API with an OpenAPI 3.1
  document (`tracepoint serve`); both HTTP transports require a bearer token, check
  Host and Origin, send no CORS headers and bind to loopback. `tracepoint
  capabilities` and `tracepoint init`.
- Phase 3: analysis. Hot buckets by the absolute and relative rules, incidents
  classified `correlated`, `storage_only`, `app_only`, `client_limited` or
  `unobserved`, culprit ranking by lead time, severity and telemetry corroboration
  with ties reported, whole-run lagged Spearman correlation, templated verdicts with
  evidence and a confidence rubric, and the Little's Law and telemetry validity
  checks. Telemetry samplers for the generator, Postgres, MySQL and Redis on the
  shared clock. `tracepoint digest` with a character budget and machine-applicable
  recommendations. `--http-threshold`, `--db-threshold`, `--redis-threshold`. Templates
  in HTTP urls, bodies and headers. `X-Tracepoint-Preflight` on preflight requests.
  The faultbox demo application, the known-answer suite (`make e2e`) and the
  integration suite (`make integration`) against real Postgres, MySQL and Redis.
- Phase 2: storage and safety. SQL and Redis runners that probe the tier while they
  load it, the template mini-language, `--set` overrides, the policy envelope with
  write and destructive-statement guards, secret redaction, the abort guard, target
  resolution and classification at preflight, `tracepoint doctor`, and `--dry-run`.
- Phase 1: the engine. A coordinated-omission-correct arrival scheduler, relative-error
  sketches with bucket sealing, the open-model executor, an HTTP runner with httptrace
  phase timing, strict configuration loading, `result.json` v1 with run-validity and
  SLO evaluation, aligned terminal tables, and the `run`, `validate`, `schema` and
  `version` commands with the `--output json` contract and the documented exit codes.
- Phase 0: the specification stored verbatim, architecture, ten ADRs, the six JSON
  Schemas with fixtures, the error and finding registry, the package skeleton, the
  build gate and CI.

### Fixed

- `executor.type: vus` ran the open model at the user count as a rate.
- `http.traceparent: true` was accepted and ignored.
- A generator paused by its host (a virtualised machine's steal time) no longer
  shows up as an incident: such buckets are set aside and reported as
  `GENERATOR_STALL`.
- A storage tier that went hot only after the application is no longer named as the
  culprit.
- A detached run ignored `--result-path`.
- The SQL and Redis runners counted reads and writes with a data race between
  workers, found by the known-answer suite.

### Changed

- Digest schema 1.1: `strain.next_window` and `caveats`.
- `scaffold_config` no longer requires `base_url` when `detect_dir` supplies one.
- Result schema 1.1: `executor.start_target` records where a load profile starts.
- Progress counts completed operations as they complete, rather than once their
  bucket seals.
- A degraded run caps verdict confidence at medium rather than forcing it to low: a
  degraded run is usable with its caveat attached, and every loopback run is degraded.
- The minimum Go toolchain is 1.26.6, the first release with no `govulncheck` findings
  against the standard-library packages this tool calls.
