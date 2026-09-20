# ADR-004: `result.json` is the system of record; how it is versioned

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §2 (system of record, contract-first), §6.7 (contract stability)

## Context

Six renderers consume one run: terminal tables, the HTML report, Markdown for pull
requests, JUnit for CI, the agent digest, and `compare`. If each reached into live
engine state, they would drift: a number would appear in the report and not the
digest, or the HTML would show a figure that could not be reproduced from the saved
file. Agents would then have to run a test to see a result, rather than reading one.

## Decision

**One document, produced once, read by everything.** `result.json` is written at the
end of a run and every renderer is a pure function of it. `tracepoint report` and
`tracepoint digest` take a result file and produce identical output to what the run
itself produced, because they run the same code over the same bytes. A consequence
worth stating plainly: **if a number is not in `result.json`, it cannot appear
anywhere** — which makes the schema the design review for every feature.

**The result is self-sufficient.** It embeds the redacted effective configuration and
the policy in force, not just a path to them, so `compare` can diff two runs' settings
and `run --from` can reproduce a run without the original file. It embeds the merged
whole-run sketches, so `compare` can compute confidence intervals without the raw
observations. It embeds the tool version and the seed, so a result found six months
later is still interpretable.

**Versioning.** `schema_version` is `MAJOR.MINOR`.

- *Additive changes are MINOR.* New fields may appear at any time. Every reader —
  ours and anyone else's — must ignore fields it does not recognise, and the schemas
  say so in prose where it matters (error classes, telemetry sample keys, event
  types).
- *Anything else is MAJOR*: removing a field, narrowing a type, changing a unit, or
  changing the meaning of an existing value.
- A **migration function is registered for every MAJOR step**, so old results stay
  readable: `migrate(1.x -> 2.0)` runs on load. `compare` and `report` migrate
  silently and note it; when no migration exists the command exits 3 with
  `RESULT_SCHEMA_UNSUPPORTED` naming both versions.
- Every emitted document is validated against its schema in tests. The schemas are
  embedded in the binary with `go:embed`, so `tracepoint schema result` always emits
  the contract that binary actually honours, and there is no way for the shipped
  schema and the shipped code to disagree.

**Units are in the names.** `p99_ms`, `duration_s`, `error_ratio`, `achieved_rps`,
`bytes_in`. No bare `latency`, no unit fields to look up, no seconds in one place and
milliseconds in another. Times inside a run are offsets in milliseconds from the
single monotonic start; wall-clock timestamps are RFC 3339 and exist for display and
log correlation only.

**Determinism.** Given the same result file, a renderer produces byte-identical
output. Maps are emitted with sorted keys, floats are rounded at the renderer rather
than carrying accumulated noise, and nothing embeds a timestamp taken at render time.
This is what makes golden-file tests meaningful, and it means a report can be
regenerated and diffed.

**Size.** A ten-minute run with three runners and 50 labels produces roughly 2,000
bucket summaries plus sketches: a few megabytes, pretty-printed. That is small enough
to keep verbatim and large enough that the HTML report downsamples for *display* only
(max-preserving, ≤ 2,000 points per series, never for analysis) and, past 5 MB,
embeds display data only while naming where the full result lives.

## Consequences

- A feature is not designed until its fields are in the schema; the schema review
  happens before the implementation, which is what "contract-first" means in practice.
- Renderers cannot accidentally depend on engine internals, because they are handed a
  decoded document and nothing else.
- Debugging a report problem means inspecting one file, and reproducing it means
  replaying that file through the renderer.
- The cost is discipline: adding a number means editing the schema, the model and the
  golden files. That cost is the point.

## Alternatives considered

- **Renderers read live engine state, and JSON export is one more renderer.** Faster
  to write, and it guarantees the exported file is a second-class citizen that drifts
  from what the terminal showed.
- **An integer `schema_version`.** Cannot express "additive, safe to ignore", so every
  new field would look like a break to a careful reader.
- **Protobuf or another binary format.** Smaller and faster, and unreadable without
  tooling. The primary consumers are humans reading a file and agents parsing JSON;
  both want text.
