# ADR-010: The tool is named `tracepoint`

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: preamble ("Working name: `strata` (rename once, use consistently)")

## Context

The specification is titled "Build TracePoint" and instructs that the working name
`strata` used throughout its body be renamed once and then used consistently. The
repository is `github.com/IshaanNene/Tracepoint`. Leaving two names alive would leak
into the module path, the binary, environment variables, HTTP headers, run
directories, the skill directory and every document — all of which are contracts
under §6.7, where a rename is a breaking change.

## Decision

The rename happens once, here, before any product code exists. Everywhere the
specification says `strata`, read `tracepoint`:

| Thing | Value |
| --- | --- |
| Module path | `github.com/IshaanNene/Tracepoint` |
| Binary and command | `tracepoint` |
| Root Go package (public facade) | `tracepoint` |
| Test helper package | `tracepointtest` |
| Command directory | `cmd/tracepoint/` |
| Environment prefix | `TRACEPOINT_` (e.g. `TRACEPOINT_POLICY`) |
| HTTP identification | `User-Agent: tracepoint/<version> (+run=<id>)`, `X-Tracepoint-Run-Id` |
| Postgres session tag | `application_name=tracepoint/<run-id>` |
| SQL comment tag | `/* tracepoint_run, <label> */` |
| Redis client name | `tracepoint-<run-id>` |
| Bundled skill | `skills/tracepoint/SKILL.md`, installed to `.claude/skills/tracepoint/` |
| MCP server entry | `tracepoint` |

The module path keeps the repository's own capitalisation so `go get` resolves
without a redirect. Go escapes the upper-case letters in the module cache, which is
ordinary and needs no handling.

"TracePoint" is the prose spelling in documentation; `tracepoint` is the spelling in
anything a machine reads.

## Consequences

- `docs/SPEC.md` keeps the word `strata` because §0.1 requires it stored verbatim.
  It is the only file in the tree that may contain the old name, and a CI check
  enforces exactly that, so the rename cannot half-happen later.
- Nothing downstream has to be renamed after this point; §6.7 treats a later rename
  as a major-version break.

## Alternatives considered

- **Keep `strata`.** Contradicts the title and the repository, and would leave the
  published binary named differently from the project people find on GitHub.
- **Use `strata` internally and `tracepoint` only in user-facing strings.** Two names
  for one thing, permanently, in a codebase whose whole selling point to agents is
  that its contracts are predictable.
