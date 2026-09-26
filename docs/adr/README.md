# Architecture decision records

Each record states the context, the decision, what follows from it, and what was
rejected. They are written when the decision is made, not afterwards, and they are
amended rather than rewritten: a superseded record keeps its text and gains a
`Superseded by` line.

Where `docs/SPEC.md` is silent, the decision and its reasoning live here.

| ADR | Decision | Status |
| --- | --- | --- |
| [001](001-in-house-scheduler.md) | An in-house, coordinated-omission-correct arrival scheduler | Accepted |
| [002](002-sketches-and-memory.md) | Relative-error sketches and the memory budget | Accepted |
| [003](003-executors-and-desugaring.md) | Two executors; `requests` desugars to single-step journeys | Accepted |
| [004](004-result-system-of-record.md) | `result.json` is the system of record; how it is versioned | Accepted |
| [005](005-correlation-methodology.md) | How hot buckets, incidents, culprits and verdicts are derived | Accepted |
| [006](006-policy-and-threat-model.md) | Policy versus config, and the threat model | Accepted |
| [007](007-dependencies.md) | Which third-party modules are allowed, and why | Accepted |
| [008](008-agent-interface.md) | JSON contract, digest, run store and the one operation registry | Accepted |
| [009](009-public-go-api.md) | The public Go API and its stability policy | Accepted |
| [010](010-name.md) | The tool is named `tracepoint` | Accepted |
| [011](011-capacity-and-compare.md) | How a capacity search runs, and what compare gates | Accepted |
