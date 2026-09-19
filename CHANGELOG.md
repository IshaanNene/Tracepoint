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

- Phase 1: the engine. A coordinated-omission-correct arrival scheduler, relative-error
  sketches with bucket sealing, the open-model executor, an HTTP runner with httptrace
  phase timing, strict configuration loading, `result.json` v1 with run-validity and
  SLO evaluation, aligned terminal tables, and the `run`, `validate`, `schema` and
  `version` commands with the `--output json` contract and the documented exit codes.
- Phase 0: the specification stored verbatim, architecture, ten ADRs, the six JSON
  Schemas with fixtures, the error and finding registry, the package skeleton, the
  build gate and CI.

### Changed

- The minimum Go toolchain is 1.26.6, the first release with no `govulncheck` findings
  against the standard-library packages this tool calls.
