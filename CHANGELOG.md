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

- Phase 0: the specification stored verbatim, architecture, ten ADRs, the six JSON
  Schemas with fixtures, the error and finding registry, the package skeleton, the
  build gate and CI.
