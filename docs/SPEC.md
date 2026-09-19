# MASTER PROMPT — Build `TracePoint`: correlated HTTP / DB / Redis load testing, agent-native

You are the principal engineer on a production-grade, open-source Go tool. This document is the spec and the source of truth. Where it is silent, choose the simplest design that satisfies it and record the decision in an ADR. Working name: `strata` (rename once, use consistently).

## 0. How we work
1. If this spec is not already at `docs/SPEC.md`, save it there verbatim before doing anything else. It must survive context compaction; re-read the relevant sections at the start of every phase.
2. Phase 0 is planning only (§12). Stop and wait for my approval before writing product code.
3. End of every phase: `make check` green → self-review against that phase's acceptance criteria with evidence (test names, command output) → update `docs/PROGRESS.md` → small conventional commits → brief report (done / deviations / risks) → wait for "continue".
4. Maintain `CLAUDE.md` (build/test commands, package map, conventions, "read docs/SPEC.md and docs/PROGRESS.md first") and `AGENTS.md` for other coding agents; keep one canonical and have the other point to it.
5. Clean-room: implement from this spec only. Do not copy code from the reference project or any other repository. Vendored third-party assets keep their LICENSE files.
6. Never claim something works without running it. If the spec conflicts with reality (library limits, platform issues), stop and present options with trade-offs.
7. Math-heavy packages (arrival schedule, sketches and quantile CIs, correlation, USL) are written test-first.
8. Use subagents for independent work (test suites, docs, reviews) when it keeps the main context clean.

## 1. Product
One question: when an application slows down under load, is the cause the application tier, the database, or the cache?

strata drives HTTP traffic (single requests or multi-step user journeys), SQL, and Redis load concurrently on one monotonic clock, records every layer into the same time buckets, samples server-side telemetry from the datastores, and reports — with evidence and a confidence level — which layer the latency came from.

Mental model: storage runners are probes plus load. They hit the DB/cache directly, so their latency is a direct health signal for that layer under combined pressure (app traffic + synthetic traffic). App latency moving with a probe's latency, corroborated by telemetry, is the evidence. It is correlational, and the tool says so.

Two first-class audiences with equal priority:
- Humans: HTML report, live terminal view, readable tables.
- Agents and automation: stable JSON contracts, MCP server, REST API, bundled agent skill, Go library, CI integrations. Every capability is reachable non-interactively with machine-readable input and output. No feature is done until its agent interface is done.

Reference capabilities to preserve: concurrent HTTP / SQL (Postgres, MySQL, SQLite) / Redis runners with weighted mixes and rate ramp; per-bucket percentiles on a shared timeline; spike correlation (correlated vs masked); multi-step journeys with JSON extraction and variable interpolation; auto-ramp capacity search with culprit attribution; "strain begins at ~N users" finder; self-contained HTML report, JSON export, aligned CLI tables, live progress; `compare` with P99 deltas, spike diff, overlaid timelines, and a non-zero exit for CI; strict YAML config; one-line installer.

Upgrades this build must deliver:
1. Coordinated-omission-correct timing: latency measured from the scheduled send time, client-side wait separated from target time (§3.3).
2. Mergeable relative-error sketches; memory independent of run length; no percentiles from tiny samples (§3.4).
3. Incidents instead of isolated spike rows, an explicit app-only class, lead/lag analysis, verdicts with evidence and confidence (§5.5).
4. Server-side telemetry (Postgres, MySQL, Redis, generator self-health) on the same clock to corroborate verdicts (§5.4).
5. Run validity: detect when the load generator, not the target, was the bottleneck (§5.5).
6. Statistically sound `compare` (quantile confidence intervals, A/A-tested) and SLO gates on single runs (§5.7).
7. Capacity search with bisection, boundary confirmation, and a Universal Scalability Law fit (§5.6).
8. Agent-native interfaces: JSON I/O contract, structured errors, async runs, context-sized digests with machine-applicable recommendations, one operation registry exposed as MCP + REST, bundled skill (§6).
9. Integration kit: Go library and test helper, GitHub Action, Docker image, `quick` mode, OpenAPI import, project detection (§7).
10. Safe by default: a human-set policy that agents cannot exceed, write and destructive-command guards, abort guard, secret redaction, offline HTML with strict CSP (§8).
11. A fault-injection known-answer suite proving every verdict class end-to-end (§10).

## 2. Engineering principles (non-negotiable)
- Go, latest stable toolchain; single static binary; linux/darwin/windows × amd64/arm64. Release builds use CGO_ENABLED=0 (SQLite via modernc.org/sqlite); `-race` test runs enable cgo.
- Hexagonal: one core (engine, runners, metrics, analysis, capacity, compare) and thin adapters (CLI, MCP, REST, Go library, GitHub Action) with no business logic. Adapters reach the core only through the public Go API (rendering helpers may be internal).
- No speculative abstraction: the only extension points are the ones named here (runner, sampler, and operation registries).
- `result.json` is the system of record. Every renderer (CLI, HTML, Markdown, JUnit, digest) is a pure function of it.
- Contract-first: JSON Schemas for config, policy, result, digest, events, and errors are drafted in Phase 0 and versioned; every emitted document is validated against its schema in tests.
- Inject Clock, RNG (seeded; seed recorded), logger (log/slog), stdout/stderr, and filesystem roots. No mutable globals.
- `context.Context` on every blocking call; clean cancellation; zero goroutine leaks (goleak).
- Errors wrapped with `%w`; typed errors carry stable codes (§6.2); no panics outside main.
- Hot path (recording one result): amortized 0 allocs/op and no global lock — per-worker shards merged on read. Benchmarked.
- Never average percentiles; merge sketches.
- Dependencies: stdlib first; each third-party dependency justified in an ADR (actively maintained, MIT/BSD/Apache-2.0, no CGO). Suggested: spf13/cobra; go.yaml.in/yaml/v3 (maintained successor of the archived gopkg.in/yaml.v3) with strict known-fields decoding; jackc/pgx/v5 via stdlib; go-sql-driver/mysql; modernc.org/sqlite; redis/go-redis/v9 (UniversalClient); tidwall/gjson; DataDog/sketches-go; Bubble Tea + Lip Gloss; github.com/modelcontextprotocol/go-sdk (official MCP SDK); testcontainers-go, alicebob/miniredis, go.uber.org/goleak, pgregory.net/rapid. No vegeta or other load-generation libraries — the scheduler is core IP (ADR-001).

## 3. Architecture

### 3.1 Layout
```
cmd/strata/               main: wiring only
strata.go, options.go     public Go API (semver-stable facade)
stratatest/               go test helper
internal/config/          YAML/JSON load, env interpolation, strict decode, defaults, validation, --set overrides, redaction, schema
internal/policy/          policy model, tighten-only merge, target classification
internal/clock/           Clock interface; real and fake
internal/schedule/        rate stages, Λ and Λ⁻¹, uniform/poisson arrivals
internal/executor/        arrival-rate (open) and vus (closed)
internal/runner/          Runner interface + registry
internal/runner/httprun/  HTTP + journeys (net/http, httptrace, extract, expect, cookies)
internal/runner/sqlrun/   database/sql runner, driver registry, read/write routing, tx units
internal/runner/redisrun/ go-redis runner, command templates, pipelining
internal/template/        precompiled mini-template, generators, feeders, static dataflow check
internal/metrics/         outcome model, error taxonomy, sketches, sharded recorders, buckets, sealing
internal/telemetry/       samplers: generator, postgres, mysql, redis
internal/analysis/        hot buckets, incidents, correlation, verdicts, validity, SLOs, strain finder
internal/capacity/        auto-ramp search, confirmation, USL, Little's Law checks
internal/compare/         diffs, quantile CIs, gates, incident matching
internal/result/          result model, schema versions, migrations, digest builder
internal/runstore/        run directories, state, events, detach/status/wait/stop, audit log
internal/ops/             agent operation registry (§6.5)
internal/adapters/mcp/    MCP adapter
internal/adapters/rest/   REST adapter
internal/render/          cli (tables, TUI, plain progress), html (go:embed), markdown, junit
internal/detect/          project detection, OpenAPI import
tools/faultbox/           demo app with fault injection (§10)
examples/  docs/  skills/strata/  action.yml
```

### 3.2 Core interfaces (shape, not final code)
```go
type Runner interface {
    Name() string
    Kind() Kind                                                // App | Storage
    Prepare(ctx context.Context) error                         // pools, warm-up, preflight
    Do(ctx context.Context, it *Iteration, rec Recorder) error // one request/query/command/journey
    Close() error
}

type Sampler interface {
    Name() string
    Sample(ctx context.Context, at time.Duration) (Sample, error)
}
```
Runners and samplers live in registries so gRPC/Kafka/Mongo can be added later without touching the engine.

### 3.3 Timing model (the heart — get this exactly right)
- One monotonic run start shared by all runners and samplers. Store offsets from it; bucket index = floor(intended_offset / bucket_width). Wall clock is for display only.
- Load profile = piecewise-linear rate stages. Arrival k fires at Λ⁻¹(k), where Λ(t) is cumulative expected arrivals. Per segment solve a·s² + b·s = c with the numerically stable root s = 2c / (b + √(b² + 4ac)), where a = (r₁ − r₀)/(2D) and b = r₀. Never sleep 1/r(t) (it drifts). Sanity check: a linear ramp to R over T yields R·T/2 arrivals during the ramp.
- `arrival: uniform | poisson` (default uniform). Poisson feeds cumulative Exp(1) sums into the same Λ⁻¹ (time rescaling).
- Each outcome records intended, dispatched, worker-start, connection-acquired, first-byte (HTTP), and end times. Derived:
  - response_time = end − intended (what a user feels; CO-corrected)
  - service_time = end − connection-acquired (what the target did)
  - client_wait = dispatch lag + queue wait + pool wait (the generator's own delay)
- Open-model executor: a bounded dispatch queue feeds `max_in_flight` workers; when full, the arrival is dropped and counted — never silently delayed without bound.
- SLO gates use response_time. Layer attribution uses service_time. If client_wait dominates a hot bucket, attribute it to the generator/config and recommend the Little's Law fix (needed in-flight ≈ rate × p99 service_time).
- SQL: acquire connections explicitly (`db.Conn`) so pool wait is timed separately from query time. HTTP: httptrace phases (DNS, connect, TLS, connection wait, TTFB, transfer; reuse flag). Redis: pool ≥ workers; surface pool stats; warn on waits.

### 3.4 Metrics and memory model
- Per (runner, label, bucket): an open sketch (1% relative accuracy) plus counters per outcome class. A bucket seals when now > bucket_end + max request timeout: it is merged into the whole-run sketches (unless in warm-up), reduced to a fixed summary {n, errors by class, p50, p90, p95, p99, p99.9, max, mean, bytes}, and freed.
- Whole-run sketches per (runner, label) and per runner are serialized into result.json for compare significance tests.
- Labels = request/step/query/command names; max 50 per runner, overflow → `other`. Never raw URLs.
- Buckets with n < min_samples (default 20) are "insufficient": displayed, never used as evidence.
- Optional raw sink: `--raw out.ndjson.gz`.
- Acceptance: a 10-minute soak at 2k req/s keeps heap flat (< 10% growth after warm-up); the record path benchmarks at 0 allocs/op amortized.

## 4. Configuration
- YAML or JSON; `-c -` reads stdin. Strict decoding: unknown keys fail with line, column, and a "did you mean" hint. `version: 1` required. `${ENV}` and `${ENV:-default}` interpolation. Durations via time.ParseDuration.
- Precedence: flags (including `--set`) > env (`STRATA_*`) > file > defaults. Policy (§8) is applied on top; config can only tighten it.
- `--set path=value` with typed values; named list items are addressable by name, e.g. `db.queries[by-id].weight=5`. Unknown paths are errors with hints.
- Load-time validation: weights > 0; stages sane; at least one runner; the `http` section holds either `requests` (sugar for single-step journeys — one app engine internally) or `journeys`, never both; every `{{var}}` a step uses must be extracted by an earlier step or provided by a feeder (static dataflow check); write/destructive classification (§8).
- Template mini-language compiled once at load: `{{var}}`, `{{feeder.column}}`, and generators `randInt a b`, `randString n`, `uuid`, `seq`, `pick a|b|c`, `now`. An unknown function is a config error. An argument consisting of exactly one generator keeps its type (ints stay ints for SQL args). If extraction fails at runtime, the step fails with `extract_failed` and the iteration stops — never send a literal `{{token}}`.
- `strata schema config` emits JSON Schema with a description and an example on every field (LLMs read these). `strata init` writes a commented starter.

Illustrative config (finalize key names in Phase 0, then keep them consistent):
```yaml
version: 1
run: { duration: 3m, bucket: 1s, warmup: 15s, seed: 42, arrival: poisson, timeout: 10s }
slo:
  http:  { p99: 250ms, error_rate: 0.01 }
  db:    { p99: 100ms }
  redis: { p99: 20ms }
http:
  executor:
    type: arrival-rate
    max_in_flight: 400
    stages: [{ duration: 30s, target: 200 }, { duration: 150s, target: 200 }]  # linear to target, then hold
  requests:
    - { name: list-items, weight: 3, method: GET, url: "${BASE_URL}/api/items" }
    - name: create-item
      weight: 1
      method: POST
      url: "${BASE_URL}/api/items"
      headers: { Content-Type: application/json }
      body: '{"title":"{{randString 12}}"}'
      expect: { status: [201] }
db:
  driver: postgres
  dsn: "${PG_DSN}"
  pool: { max_open: 20 }
  executor: { type: arrival-rate, rate: 40 }   # shorthand: constant for run.duration
  queries:
    - { name: by-id, type: read, weight: 5, sql: "SELECT * FROM items WHERE id = $1", args: ["{{randInt 1 100000}}"] }
redis:
  addr: "${REDIS_ADDR}"
  executor: { type: arrival-rate, rate: 100 }
  commands:
    - { name: get-session, weight: 4, cmd: [GET, "session:{{randInt 1 50000}}"] }
telemetry: { postgres: true, redis: true }
```

## 5. Features

### 5.1 Runners
HTTP / journeys:
- Weighted requests; journeys with ordered, named steps; `extract` (gjson paths); `expect` (status set, optional JSON-path equals/exists, max body size); per-step timeout; think time (constant, uniform, exponential); per-VU cookie jar; CSV feeders (sequential, random, unique).
- Journey duration excludes think time. The app reference series for correlation is the merged app-side p99 per bucket; per-label thresholds may override.
- Tuned Transport (idle conns ≥ max_in_flight, optional HTTP/2, keep-alive toggle, explicit TLS options — insecure_skip_verify is printed in every report). Bodies are always drained and closed.
- Identifiable traffic: `User-Agent: strata/<version> (+run=<id>)`, `X-Strata-Run-Id`, optional W3C `traceparent` injection so APMs can filter and join test traffic.

SQL:
- postgres (pgx stdlib), mysql, sqlite; aliases normalized; an unknown driver error lists the compiled-in drivers.
- Queries: name, weight, `type: read|write` (authoritative; heuristic fallback), args with generators, optional multi-statement `tx` unit, per-query timeout, optional prepared statements. Reads iterate and close every row.
- Pool: max_open, max_idle, lifetimes; defaults = worker count; warn when max_open exceeds the server's max_connections.
- Session tagging: Postgres `application_name=strata/<run-id>`; optional sqlcommenter-style `/* strata_run, label */` comments so pg_stat_activity, pg_stat_statements, and slow logs can attribute test traffic.

Redis:
- UniversalClient (single, cluster, sentinel), TLS, commands as argument arrays with generators, optional pipelining, read/write classification from command metadata, `CLIENT SETNAME strata-<run-id>`.

### 5.2 Executors
- `arrival-rate` (open): rate stages or `rate` shorthand, `max_in_flight`, dropped arrivals counted.
- `vus` (closed): VU-count stages; `pick: per-iteration` (default) or `per-vu`. Journeys may run under either executor.
- Warm-up is excluded from summaries and baselines (shaded in charts).
- Graceful stop: stop arrivals at the end, drain in-flight up to `grace`, then cancel. First SIGINT = graceful stop with a partial result marked interrupted; second = immediate exit.

### 5.3 Preflight and doctor
- Preflight (always, before any load): resolve and classify targets, TCP connect, one request per HTTP label, DB ping plus `PrepareContext` on every statement (catches typos without executing), Redis PING.
- `strata doctor`: preflight + telemetry permission checks (e.g. pg_read_all_stats, Redis latency monitor) + generator host checks (open-file limit vs planned connections, ephemeral port range, CPU count). Every finding has a code, a severity, and a fix.

### 5.4 Telemetry samplers (opt-in, read-only, one dedicated connection each, fail-soft)
- Generator self-health (always on): CPU, goroutines, GC pauses and scheduler latency (runtime/metrics), dispatch lag p99, in-flight per runner.
- Postgres: pg_stat_activity by state and wait_event_type (strata sessions vs others via application_name), waiting locks, pg_stat_database deltas (commits, rollbacks, cache hit ratio, deadlocks, temp bytes); optional end-of-run top-N from pg_stat_statements.
- MySQL: SHOW GLOBAL STATUS deltas (threads running/connected, row lock waits and time, buffer pool reads vs read requests).
- Redis: INFO deltas (ops/sec, connected and blocked clients, memory, hits/misses, evictions, rejected connections); LATENCY LATEST when enabled.
- Missing permissions degrade gracefully ("unavailable: <reason>"). Samples use the shared offset clock.

### 5.5 Analysis (pure functions over result data)
1. Hot buckets per runner (sufficient samples only), with the reason recorded:
   - absolute: p99 service_time > threshold (from `slo`, else `--<runner>-threshold`, default 100ms);
   - relative: modified z-score 0.6745·(x − median)/MAD > 3.5 against a rolling median of the previous 30 steady buckets (needs ≥ 10), and p99 ≥ 3× that median, and ≥ 5ms above it.
2. Incidents: merge each runner's hot buckets with gaps ≤ 1; overlap across runners with ±1 bucket tolerance. Classes:
   - `correlated`: app hot and at least one storage layer hot;
   - `storage_only` (masked): storage hot while the app stays within SLO — a bottleneck not yet reaching users;
   - `app_only`: app hot while every storage probe is healthy with sufficient samples → application tier;
   - `client_limited`: client_wait dominates → generator/config, not the target;
   - `unobserved`: app hot, storage probes had insufficient data.
3. Culprit ranking inside correlated incidents: lead time, severity (p99 ÷ threshold), telemetry corroboration (pool saturation, lock waits, blocked clients, hit-ratio drops). Ties are reported as ties.
4. Whole run: Spearman ρ between the app p99 series and each storage p99 series at lags −3…+3 buckets; report best lag, ρ, and the bottleneck lean.
5. Verdict: deterministic templated narrative + structured evidence + confidence (high/medium/low from sample sizes, incident count, |ρ|, corroboration). Always states that correlation is not proof, and ends with concrete next steps.
6. Validity: `valid` / `degraded` / `invalid`.
   - invalid: dispatch lag p99 > max(5ms, 5% of bucket) (the generator fell behind), or dropped arrivals > 1% while service_time stayed flat (client-side cap).
   - degraded: dropped arrivals with rising service_time (report a `target_saturated` finding), loopback targets (same-host caveat, auto-detected), > 5% HTTP 429 ("you are measuring the rate limiter"), insecure TLS, telemetry unavailable, Little's Law inconsistency > 20%.
7. SLO evaluation: per runner p95/p99/error-rate budgets on response_time → pass/fail.
8. Strain finder (ramping runs): map buckets to measured in-flight/active VUs; first sustained strain = app p99 > 2× the median of early steady buckets for ≥ 3 consecutive sufficient buckets → "strain begins at ~N users (~R req/s)" or "no strain up to ~N", plus the recommended next capacity window.

### 5.6 Capacity search
- Knob: `concurrency` or `rate`. Double from the start value until a level breaks or the cap is reached, then refine between last-ok and first-broken by bisection (default) or linear fill (`refine: linear:N`) until the gap ≤ resolution (default max(1, 5%)).
- Each level: settle period discarded (default 20% of the step), steady measurement window, cooldown between levels; connections stay warm across levels.
- Break = any SLO breach or a throughput plateau (achieved < 90% of offered). Culprit per broken level via §5.5.
- Confirmation: re-run last-ok and first-broken once; if either flips, report an unstable boundary range instead of a point.
- Early abort when the error rate exceeds 50%.
- USL fit on (measured mean concurrency, throughput) with ≥ 4 ok levels: X(N) = λN / (1 + σ(N−1) + κN(N−1)); constrained least squares (σ, κ ≥ 0; outer 1-D search over λ, inner linear solve). Report σ, κ, predicted peak N* = √((1−σ)/κ), and R²; hide the fit when R² < 0.9.
- Output: level table, knee, knob vs p99 and throughput chart, USL overlay.

### 5.7 Compare
- Inputs: two result files or run IDs. Schema-version check (migrate, or exit 3). Warn when configs differ, listing the differing keys.
- Per runner and label: p50/p95/p99, error rate, and throughput deltas.
- Significance: distribution-free order-statistic CIs for p99 from each run's sketch (ranks n·q ± 1.96·√(n·q·(1−q))); a rank interval outside [1, n] means insufficient data. A change counts only when the intervals don't overlap (conservative by design).
- Gates (configurable; any breach → exit 1): budget crossing (current p99 > budget while baseline ≤ budget); relative increase (> 10% and > 5ms, significant); error-rate increase (> 0.5 percentage points).
- Incident diff by runner and overlapping offset window (±2 buckets): new / fixed / worsened / improved / unchanged.
- Formats: table, json, markdown (PR comment), junit, HTML with overlaid timelines.

### 5.8 Human outputs
- Per-run directory (§6.4); `--result-path`, `--report-path`, `--out-dir` override locations.
- HTML: one file, fully offline — chart library vendored via go:embed, zero network requests, strict CSP (default-src 'none'; inline `<script>`/`<style>` allowed only by sha256 hashes computed at render time), data embedded in `<script type="application/json">` via encoding/json with HTML escaping. Sections: validity and SLO badge; verdict with evidence and confidence; incidents; shared timeline (p50/p95/p99 and response/service toggles; incidents, warm-up, and ramp shaded; in-flight on a secondary axis); per-runner and per-label tables with error classes and status histograms; HTTP phase breakdown; telemetry panels; capacity section; methodology and caveats; Export JSON (when the full result exceeds 5 MB, embed display data only and say where result.json lives). Max-preserving downsampling for display (≤ 2,000 points per series), never for analysis. Colorblind-safe palette, dark mode, keyboard accessible.
- CLI: ANSI-aware aligned tables (NO_COLOR respected), incidents, one-line verdict, validity, artifact paths. Live Bubble Tea view on a TTY only; otherwise one slog line every 5s (`--log-format text|json`).

## 6. Agent-native interface (first-class, not a wrapper)

### 6.1 CLI contract for any agent with a shell
- `--output json` on every one-shot command: stdout carries exactly one JSON document (the result or an error envelope); logs and progress go to stderr. With `--events -`, stdout is NDJSON only and the final `run.finished` event embeds the digest.
- Never block on input: no prompts when stdin is not a TTY, when `CI` is set, or with `--non-interactive`. TUI and colors only on a TTY with human output.
- Exit codes: 0 success · 1 SLO breach or regression · 2 usage/config/policy refusal · 3 preflight/runtime failure or aborted · 4 run invalid (unless `--allow-invalid`) · 5 `wait` timed out while the run continues. Precedence when several apply: 2 > 3 > 4 > 1.
- `strata capabilities --output json`: a manifest generated from the command tree and registries (never hand-written): commands, flags with types and defaults, exit codes, error codes, runners, drivers, agent operations, contract versions, deprecations.

### 6.2 Structured errors
Every error carries a stable code and a fix:
```json
{"error":{"code":"CONFIG_UNKNOWN_FIELD","message":"unknown field \"metod\"","path":"/http/requests/0/metod","line":12,"column":7,"hint":"did you mean \"method\"?","docs":"docs/CONFIG.md#http-requests"}}
```
Codes are listed in `docs/ERRORS.md` and in `capabilities`; renaming a code is a breaking change.

### 6.3 Digest: the agent-facing result
`strata digest <run|result.json> [--budget-chars 4000]` returns a compact, prioritized summary that fits a context window: validity → SLO → verdict (bottleneck, confidence, one-paragraph summary, evidence) → top incidents → key numbers per runner → recommendations → artifact paths. snake_case fields with units in the name (`p99_ms`, `error_ratio`, `achieved_rps`); numbers are numbers; no ANSI, no filler prose. When over budget, drop the lowest-priority content and set `truncated: true` with instructions for fetching more.
Recommendations are machine-actionable:
```json
{"id":"raise-max-in-flight","why":"client_wait dominated 7 hot buckets; Little's Law needs ~240 in flight","action":"rerun","overrides":["http.executor.max_in_flight=240"]}
```
`strata run --from <run-id> --set ...` re-runs the same effective config plus overrides, closing the agent loop.

### 6.4 Run store and async runs (agent tool calls time out; load tests must not)
- Each run directory `runs/<UTC timestamp>-<short id>/` holds `state.json` (status, progress, heartbeat, pid), `events.ndjson` (versioned, append-only: run.started, bucket.sealed, incident.opened/closed, level.started/completed, warning, run.finished), `result.json`, `digest.json`, `report.html`, `config.effective.yaml` (with `${ENV}` references preserved; secret values supplied inline are stored redacted and must be re-supplied on `--from`), and `run.log`.
- `strata run --detach` returns `{run_id, run_dir}` only after validation and preflight pass (config errors stay synchronous), then continues in a background process.
- `strata status|wait|stop|list`: `wait --timeout` exits with the run's final code, or 5 if it is still running; `stop` writes a STOP sentinel the run polls (portable and graceful; `--force` kills); a stale heartbeat with a dead pid reports `lost`. `strata gc --keep N` prunes old runs.
- Append-only audit log `runs/audit.ndjson`: actor (CLI user, MCP/REST client identity, or library caller), config hash, targets, peak rates, outcome.

### 6.5 Agent operations: defined once, exposed everywhere
- `internal/ops` registry: each operation has a name, an LLM-oriented description (what it does and when to use it), input and output JSON Schemas, annotations (read-only, idempotent, destructive, open-world), and a handler over the public API. MCP tools, REST endpoints, and `capabilities` are generated from it; a parity test guarantees they never drift from each other or from the CLI.
- Operations: get_policy · scaffold_config · validate_config · plan_run · start_run (async; `config` inline or `config_path`, plus `overrides`) · get_run_status · wait_for_run (bounded, ≤ 50s per call) · stop_run · get_run_digest · get_run_section (paginated: incidents, labels, timeline window, telemetry, levels) · compare_runs · list_runs.
- `strata mcp`: official Go SDK; stdio by default, optional Streamable HTTP. Tools return structured content plus a short text summary for clients without structured-output support. Resources: JSON Schemas, run digests and results, methodology and config docs. Prompts: `hunt_bottleneck`, `capacity_check`, `regression_gate`. Build only on tools, resources, and prompts — do not depend on roots, sampling, or logging, which the current MCP spec revision deprecates.
- `strata serve`: REST/JSON API with an OpenAPI 3.1 document generated from the same registry, for agent frameworks and services that don't speak MCP.
- Both HTTP transports bind 127.0.0.1 by default, require a bearer token (generated into a 0600 file if not supplied), validate Host and Origin headers (DNS-rebinding and localhost-CSRF protection), and enable no CORS.
- Policy is fixed when the server starts (`--policy` file or flags) and no call can exceed it; violations return `POLICY_DENIED` naming exactly what a human must change. Server-mode defaults: private and loopback targets only, no writes, ≤ 500 req/s per runner, ≤ 10 minutes per run, one active run at a time.
- Target-supplied strings (error messages, headers) are untrusted input for downstream LLMs: truncate to 200 characters, strip control characters and ANSI, label them as target-supplied, and never include response bodies in digests or operation results.

### 6.6 Bundled agent skill and one-command setup
- `skills/strata/SKILL.md` (name and description frontmatter; reference files for config recipes, result interpretation, failure modes), embedded in the binary via go:embed so the skill always matches the binary version.
- Workflow it teaches: detect the project (`strata init --detect`) → ask the human which targets and environments are allowed, whether writes are OK, the SLOs, and the time budget — never assume, and never target production without explicit allowlisting by a human → validate → doctor → dry-run plan → 30s smoke run (must be `valid`) → baseline run → read the digest first and drill into sections only as needed → follow recommendations to test hypotheses → report verdict, confidence, evidence, caveats, and the report path → gate future changes with compare. Anti-patterns: interpreting `invalid` runs, raising rates blindly, loosening policy, claiming causation, running more than one test against the same target at once.
- `strata agent setup --client claude|generic --scope project|user [--apply]`: installs the skill into `.claude/skills/strata/` (project) or `~/.claude/skills/strata/` (user). Project scope merges a server entry into `.mcp.json` without clobbering existing entries; user scope prints `claude mcp add --transport stdio --scope user strata -- strata mcp` and runs it only with `--apply`. Other MCP clients get a generic command + args snippet.

### 6.7 Contract stability
- Contracts: CLI commands and flags, exit codes, error codes, JSON Schemas (config, policy, result, digest, events), agent operations (MCP tools and REST endpoints), and the public Go API.
- SemVer. Within a major version, additive changes only. Deprecations appear in CHANGELOG, `capabilities.deprecations`, and a warning event for at least one minor version before removal in the next major.
- CI enforcement: golden files for every JSON output, schema validation of every emitted document, snapshot tests of the MCP tool list and OpenAPI document, a Go API compatibility check (apidiff or gorelease), and the ops parity test.

## 7. Integration kit
- `strata quick <url> [--db-dsn …] [--redis …] [--rate 50] [--duration 30s]`: zero-config smoke test with probes and correlation. The DB probe query is configurable (default `SELECT 1`, with its blind spots stated in the digest). `--print-config` emits the equivalent YAML.
- `strata init --from-openapi spec.yaml --base-url URL`: weighted requests from safe methods only (GET/HEAD by default), generators for path parameters inferred from schemas, TODOs for anything uncertain. `strata init --detect <dir>`: reads compose files, env var names (never values — it writes `${DATABASE_URL}`), and OpenAPI specs; returns a config plus a list of inferences with confidence.
- Go library: `strata.LoadConfig`, `strata.Plan`, `strata.Run(ctx, cfg, strata.WithEvents(fn), strata.WithPolicy(p))`, `strata.Compare`, `strata.Digest`. `stratatest.Run(t, cfg)` fails a Go test on an SLO breach or an invalid run.
- GitHub Action (`action.yml`): inputs for config, baseline, and gates; downloads the release binary with checksum verification; writes the Markdown summary to the job summary; uploads result.json and report.html as artifacts; optional PR comment.
- Docker: distroless, non-root image; a docker-compose demo (faultbox + Postgres + Redis) that runs the README quickstart in under 60 seconds.
- `docs/INTEGRATIONS.md` with copy-paste recipes: Claude Code (MCP + skill), any shell-capable agent (JSON contract), non-MCP agent frameworks (REST + OpenAPI), GitHub Actions, GitLab CI, Docker/Compose, Kubernetes Job, Go tests.

## 8. Safety and security
- Policy vs config: config is what a user or agent writes; policy is what a human grants (`--policy` file, `STRATA_POLICY`, or flags). Config `safety` can only tighten policy. ADR-006 includes a lightweight threat model.
- Targets: every host is resolved; loopback, private, and link-local addresses are allowed; public targets are refused unless allowlisted (exit 2 with instructions). No interactive confirmations — explicit allowlisting only.
- Writes: SQL/Redis writes require `allow_writes`. Destructive or admin statements (DROP, TRUNCATE, ALTER, FLUSHALL, FLUSHDB, SHUTDOWN, CONFIG SET, DEBUG, SCRIPT FLUSH, …) are refused unless `allow_dangerous`; warn on O(N) blockers such as KEYS.
- Abort guard (default on): stop when connection errors, timeouts, or 5xx exceed 50% for 10s (4xx and 429 don't count).
- Secrets: env interpolation; redact DSN passwords, Authorization/Cookie/Set-Cookie and configured headers, and args marked `secret: true` everywhere — logs, result.json, HTML, digests, events, errors, operation results. Test: a sentinel secret never appears in any artifact.
- HTML safety test: a URL containing `</script><img src=x onerror=alert(1)>` renders inert.
- The installer verifies SHA-256 checksums, plus cosign signatures when cosign is available.

## 9. CLI surface
`run` (`--detach`, `--dry-run`, `--set`, `--from`, `--output json`, `--events`) · `quick` · `status` · `wait` · `stop` · `list` · `gc` · `digest` · `report` (HTML/Markdown/JUnit from a result) · `compare` · `validate` · `doctor` · `init` · `schema <config|policy|result|digest|events|error>` · `capabilities` · `mcp` · `serve` · `agent setup` · `version`. Every command's `--help` includes examples. Documented in `docs/CLI.md`.

## 10. Testing
- Table-driven unit tests. Property tests (rapid): arrival counts within ±1 of ∫rate for random stages; Λ⁻¹ monotonic; sketch merge associative and commutative; quantile error within the configured relative accuracy vs exact sorting; incident merging idempotent.
- Fake clock for scheduler/executor determinism; `-race` everywhere; goleak in every package's TestMain.
- Fuzzing: config parser, `--set` parser, template compiler, JSON extraction, SQL/Redis classifiers.
- Golden files for CLI tables, Markdown, JSON outputs, and digests (`-update` flag); every emitted JSON document validated against its schema.
- Integration (tag `integration`): testcontainers Postgres, MySQL, Redis; miniredis for fast unit tests.
- Known-answer e2e (tag `e2e`, `make e2e`) with `tools/faultbox` — an app over Postgres + Redis with an admin port that injects faults at known times. Required outcomes:
  - Postgres table lock on a table both the endpoint and the probe use → `correlated`, culprit db, within ±1 bucket.
  - Lock on a table only the probe uses → `storage_only`.
  - Handler delay → `app_only`.
  - Redis DEBUG SLEEP (container started with `--enable-debug-command yes`) → `correlated`, culprit redis.
  - `max_in_flight: 2` against a slow endpoint → `client_limited`; the target is not blamed.
  - Clean run → zero incidents. Two clean runs compared (A/A) → no regression. +50ms handler delay → significant regression, exit 1.
  - The same flow driven through the MCP server with the SDK's client, and through REST: validate → plan → start → wait → digest → compare.
- Soak (tag `soak`): 10 minutes at 2k req/s with a flat heap. Benchmarks for the record path, scheduler, and sketch merge (`make bench` + benchstat); document the max sustainable rate per runner against a no-op target in `docs/PERFORMANCE.md`.
- Coverage gates: ≥ 85% for schedule, metrics, analysis, capacity, compare; ≥ 70% overall. Nightly CI runs the known-answer suite 5× to catch flakes.

## 11. Delivery
- Makefile: `check` (gofmt/goimports check, vet, golangci-lint, `test -race`, govulncheck), `test`, `integration`, `e2e`, `bench`, `build`, `release-dry`.
- golangci-lint: errcheck, govet, staticcheck, revive, gosec, errorlint, bodyclose, noctx, contextcheck, exhaustive, gocritic, unparam.
- GitHub Actions: lint + test matrix (linux, macOS, windows), integration and e2e on linux, coverage gates, govulncheck, contract checks (§6.7), Dependabot or Renovate.
- GoReleaser: all targets, `-trimpath`, version ldflags, checksums, SBOM (syft), keyless cosign signatures, container image; optional Homebrew tap.
- Docs: README (30-second quickstart, including the one-command agent setup), `docs/CONFIG.md` (generated from the schema plus prose), `docs/CLI.md`, `docs/ERRORS.md`, `docs/INTEGRATIONS.md`, `docs/METHODOLOGY.md` (coordinated omission, open vs closed models, response vs service time, sketches, incident rules, limits of correlation, same-host caveat, Little's Law, USL), `docs/ARCHITECTURE.md`, `docs/PERFORMANCE.md`, `docs/adr/`, CHANGELOG, SECURITY, CONTRIBUTING.
- Examples: light, heavy, journeys-weighted, probe-only, auto-ramp, compare-in-ci, mcp-policy.

## 12. Phases (each ends with the §0.3 routine)
0. Plan: restate the product in 10 lines; list assumptions and open questions; ARCHITECTURE.md; ADRs (001 in-house CO-correct scheduler · 002 sketch choice and memory math · 003 executors and http→journeys desugaring · 004 result.json as system of record and schema versioning · 005 detection and correlation methodology · 006 policy, safety, threat model · 007 dependencies · 008 agent interface: JSON contract, digest, run store, ops registry · 009 public Go API and stability policy); draft every JSON Schema; CLAUDE.md, AGENTS.md, PROGRESS.md; skeleton, Makefile, lint config, CI stub. STOP for approval.
1. Engine slice: clock, schedule math, sketches, sharded recorder, sealing, arrival-rate executor, HTTP runner, result.json v1, CLI table, `--output json`, error envelope, exit codes, `run`/`validate`/`schema`/`version`, graceful shutdown. E2E against an httptest server.
2. Storage and safety: SQL and Redis runners, templates and generators, `--set`, policy, write/dangerous guards, abort guard, redaction, preflight, `doctor`, `--dry-run` plan JSON.
3. Analysis: telemetry samplers, hot buckets, incidents, culprit, lagged Spearman, verdicts, validity, SLO gates, digest with recommendations; faultbox and the known-answer suite.
4. Agent interfaces: run store, `--detach`/status/wait/stop/list/gc, NDJSON events, audit log, ops registry, `capabilities`, MCP server, REST server, parity tests.
5. Human interfaces: HTML, Markdown, JUnit renderers, `report`, TUI and plain progress.
6. Journeys and onboarding: vus executor, extract/expect, feeders, think time, cookies, static dataflow check, strain finder; `quick`, `init --detect`, `init --from-openapi`.
7. Capacity and compare: auto-ramp with confirmation and USL; compare with CIs, gates, incident diff, formats; A/A and regression e2e.
8. Integration and release: Go facade and `stratatest`, GitHub Action, Docker/Compose, bundled skill and `agent setup`, contract-stability CI, fuzzing, soak, GoReleaser, installer, docs, examples. Then dogfood with an agent: spawn a subagent that has only the MCP server and the skill (no source access) and must diagnose every faultbox scenario. Log each point of friction (unclear tool description, missing field, confusing error, oversized output) in `docs/agent-dogfood.md`, fix it, and repeat until the subagent diagnoses every scenario correctly without hints.

## 13. Definition of done
- `make check`, `make integration`, and `make e2e` green on a clean clone.
- The report opens offline with zero network requests (a test scans the HTML for external URLs).
- A generator-limited run is flagged invalid; a same-host run carries the caveat.
- A/A compare shows no regression; the injected +50ms regression exits 1.
- Soak heap flat; record path 0 allocs/op.
- No sentinel secret in any artifact; the XSS test renders inert.
- An agent using only MCP + the skill, and a shell-only agent using `--output json` + `digest`, each diagnose every faultbox scenario correctly.
- The README quickstart works verbatim in under 60 seconds.

## 14. Non-goals (design for, don't build)
Browser/E2E testing, WebSockets/streaming, distributed multi-node generation (keep the engine coordinator-friendly), gRPC/Kafka/Mongo runners (registries ready), OpenTelemetry trace ingestion for per-request causal attribution (the natural v2 — note it in ARCHITECTURE.md), live Prometheus/OTLP export.

Only make changes in TracePoint folder
