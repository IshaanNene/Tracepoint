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

- The SQL and Redis runners counted reads and writes with a data race between
  workers, found by the known-answer suite.

### Changed

- A degraded run caps verdict confidence at medium rather than forcing it to low: a
  degraded run is usable with its caveat attached, and every loopback run is degraded.
- The minimum Go toolchain is 1.26.6, the first release with no `govulncheck` findings
  against the standard-library packages this tool calls.
