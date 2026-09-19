# ADR-008: JSON contract, digest, run store and one operation registry

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §1 (two audiences, equal priority), §6 (agent-native interface)

## Context

Agents are a first-class audience, not a wrapper bolted on at the end. That has four
concrete consequences that a human-only tool never faces.

An agent reads with a **context budget**: a 4 MB `result.json` is not "detailed", it is
unusable, and an agent that has to page through it will summarise it badly. An agent's
tool call **times out** in tens of seconds, while a useful load test runs for minutes.
An agent **branches on strings**, so an error message that reads well and changes
wording between versions is worse than a stable code. And an agent will be reached by
**whatever the target says**, so anything a target emits is untrusted input.

Meanwhile the same capability has to be available through a CLI, an MCP server and a
REST API. Implemented three times, they drift, and the drift lands on the audience
least able to cope with it.

## Decision

### One registry, three surfaces

`internal/ops` defines each operation once: a name, a description written for a model
(what it does *and when to use it*), input and output JSON Schemas, annotations
(read-only, idempotent, destructive, open-world), and a handler that calls the public
Go API. MCP tools, REST endpoints and the `capabilities` manifest are **generated**
from it, and a parity test asserts the three cannot diverge from each other or from
the CLI. `capabilities` is generated from the cobra command tree and the registries
for the same reason: a hand-written manifest is a lie waiting to happen.

Operations: `get_policy`, `scaffold_config`, `validate_config`, `plan_run`,
`start_run`, `get_run_status`, `wait_for_run`, `stop_run`, `get_run_digest`,
`get_run_section`, `compare_runs`, `list_runs`.

### The CLI is an agent interface too

Not every agent speaks MCP; most have a shell. So `--output json` on every one-shot
command puts **exactly one JSON document** on stdout — the result or an error
envelope — with logs and progress on stderr. With `--events -`, stdout is NDJSON only
and the terminal `run.finished` event embeds the digest, so a streaming consumer never
has to open a second file. Nothing ever prompts when stdin is not a TTY, when `CI` is
set, or under `--non-interactive`.

Exit codes are part of the contract: 0 success, 1 SLO breach or regression, 2 usage or
policy refusal, 3 preflight or runtime failure, 4 run invalid, 5 `wait` timed out
while the run continues. Precedence when several apply: 2 > 3 > 4 > 1 — a config that
was refused never reports an SLO verdict it did not produce. Error codes are stable
identifiers; renaming one is a breaking change under §6.7.

### Async runs, because tool calls time out

A run is a resource on disk. `runs/<UTC timestamp>-<short id>/` holds `state.json`,
append-only `events.ndjson`, `result.json`, `digest.json`, `report.html`,
`config.effective.yaml`, `run.log`.

`run --detach` returns `{run_id, run_dir}` **only after validation and preflight
pass**, so the caller learns about a typo synchronously and only a genuinely running
test goes to the background. `wait_for_run` is bounded at 50 seconds per call and
returns "still running" rather than blocking past a tool-call deadline, so an agent
polls in a loop it controls. `stop` writes a sentinel file the run polls, which is
portable and graceful where signals are neither; `--force` kills. A stale heartbeat
with a dead pid reports `lost` rather than hanging.

`config.effective.yaml` preserves `${ENV}` *references* rather than resolved values,
so a run can be reproduced without its secrets being written to disk; a secret
supplied inline is stored redacted and must be re-supplied on `--from`.

### The digest is the primary agent output

`result.json` is the system of record; the digest is what an agent should read. It is
prioritised, not merely shortened: validity, SLO, verdict, incidents, key numbers,
recommendations, artifact paths — in that order, so an agent that stops reading early
has still read the most important thing. Field names carry units (`p99_ms`,
`error_ratio`, `achieved_rps`); numbers are numbers; there is no ANSI and no prose
filler. Over budget, the lowest-priority content is dropped and `truncated: true`
appears with instructions for fetching the rest through `get_run_section`.

**Validity is first because an invalid run must not be interpreted**, and an agent
reading top-down will meet that fact before it meets a verdict it might otherwise
quote.

Recommendations are **executable**, not advice:

```json
{"id":"raise-max-in-flight","why":"client_wait dominated 7 hot buckets; Little's Law needs ~240 in flight",
 "action":"rerun","overrides":["http.executor.max_in_flight=240"]}
```

With `run --from <run-id> --set ...` re-running the same effective config plus
overrides, the loop closes: an agent can form a hypothesis, apply the tool's own
recommendation and compare the outcome, without reconstructing a config from prose.

### Target output is untrusted

Strings that came from a target are truncated to 200 characters, stripped of control
characters and ANSI, and labelled as target-supplied. Response bodies never appear in
a digest or an operation result. See ADR-006, threat 5.

## Consequences

- Adding an operation means adding one registry entry; forgetting to expose it
  somewhere is a test failure, not a support ticket.
- The MCP adapter builds only on tools, resources and prompts, avoiding roots,
  sampling and logging, which the current spec revision deprecates.
- Tools return structured content *and* a short text summary, since not every client
  renders structured output.
- The skill and the digest are versioned with the binary — the skill is embedded with
  `go:embed` — so an agent cannot be following instructions for a different release.

## Alternatives considered

- **MCP only.** Excludes every shell-capable agent, which today is most of them.
- **Return `result.json` from tool calls.** Blows a context window and buries the
  answer in the middle of it.
- **Synchronous runs only.** Caps useful tests at the tool-call timeout, which is
  roughly one minute — shorter than a warm-up.
- **Hand-write the MCP tool list and the OpenAPI document.** Three sources of truth
  and a guaranteed drift, discovered by whoever is least equipped to debug it.
