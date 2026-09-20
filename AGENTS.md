# AGENTS.md

This repository keeps one canonical set of instructions for coding agents, in
**[`CLAUDE.md`](CLAUDE.md)**. Read it — it applies whatever agent you are.

The short version:

- Read [`docs/SPEC.md`](docs/SPEC.md) (the source of truth, stored verbatim),
  [`docs/PROGRESS.md`](docs/PROGRESS.md) (what is actually built) and
  [`docs/adr/`](docs/adr/) (decisions already made) before changing anything.
- `make check` is the gate and must be green before any commit.
- `result.json` is the system of record; every renderer is a pure function of it.
- Never average percentiles; merge sketches.
- Contracts — CLI flags, exit codes, error codes, JSON Schemas, agent operations and
  the public Go API — are versioned. Within a major version, additive changes only.

Not to be confused with the *agent-facing product surface*, which is a different thing
in this repository: the MCP server, the REST API, the digest and the bundled skill that
TracePoint ships for agents to **use**. That is specified in §6 of the spec and
designed in [ADR-008](docs/adr/008-agent-interface.md).
