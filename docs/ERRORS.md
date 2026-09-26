# Error and finding codes

Every failure TracePoint reports carries a stable code. **Branch on the code, never on
the message** — messages are written for humans and may be reworded in any release,
whereas renaming a code is a breaking change under §6.7. The full registry is also
emitted by `tracepoint capabilities --output json`, so a client can enumerate it
without reading this file.

Shape of a failure (`schemas/error.schema.json`):

```json
{"error":{"code":"CONFIG_UNKNOWN_FIELD","message":"unknown field \"metod\"",
          "path":"/http/requests/0/metod","line":12,"column":7,
          "hint":"did you mean \"method\"?","docs":"docs/CONFIG.md#http-requests",
          "exit_code":2,"retriable":false}}
```

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | SLO breach or regression — the tool worked, the target did not meet its budget |
| 2 | Usage, configuration or policy refusal — nothing ran |
| 3 | Preflight or runtime failure, or aborted |
| 4 | Run invalid: the generator, not the target, set the pace (unless `--allow-invalid`) |
| 5 | `wait` timed out while the run continues |

When several apply, precedence is **2 > 3 > 4 > 1**: a configuration that was refused
never reports a verdict it did not produce, and an invalid run never reports an SLO
result that cannot be believed.

## Errors

### Configuration — exit 2, never retriable

| Code | Meaning and fix |
| --- | --- |
| `CONFIG_NOT_FOUND` | The configuration file does not exist. Check the path, or pass `-c -` to read stdin |
| `CONFIG_PARSE` | The file is not valid YAML or JSON. Carries line and column |
| `CONFIG_UNKNOWN_FIELD` | A key the schema does not define. Carries line, column and a did-you-mean hint |
| `CONFIG_MISSING_FIELD` | A required key is absent |
| `CONFIG_INVALID_VALUE` | A value is out of range or the wrong type — a zero weight, a malformed duration, an error rate above 1 |
| `CONFIG_VERSION_UNSUPPORTED` | `version` is not 1 |
| `CONFIG_NO_RUNNER` | None of `http`, `db` or `redis` is present; there is nothing to run |
| `CONFIG_REQUESTS_AND_JOURNEYS` | `http` holds both keys. They imply different intents, so pick one |
| `CONFIG_DUPLICATE_NAME` | Two labels share a name within a runner; statistics would merge silently |
| `CONFIG_ENV_UNSET` | A `${ENV}` reference has no value and no `:-default` |
| `CONFIG_STAGES_MISMATCH` | Executor stage durations do not sum to `run.duration` |
| `CONFIG_TEMPLATE_SYNTAX` | A `{{...}}` token is malformed |
| `CONFIG_TEMPLATE_UNKNOWN_FUNC` | A generator that does not exist. The message lists the ones that do |
| `CONFIG_UNDEFINED_VARIABLE` | A step uses `{{var}}` that no earlier step extracts and no feeder provides — the static dataflow check (§4) |
| `CONFIG_FEEDER_NOT_FOUND` | A `{{feeder.column}}` names a feeder or column that is not declared |
| `SET_PARSE` | A `--set` argument is not `path=value` |
| `SET_UNKNOWN_PATH` | The path does not exist in the config schema. Carries a hint |
| `SET_TYPE_MISMATCH` | The value cannot be coerced to the field's type |
| `SET_INDEX_NOT_FOUND` | `list[name]` names an item that does not exist |

### Policy — exit 2, never retriable

All of these name exactly what a **human** would have to change. An agent's correct
response is to report the refusal, not to work around it.

| Code | Meaning and fix |
| --- | --- |
| `POLICY_DENIED` | Generic refusal; `details` names the specific limit |
| `POLICY_PARSE` | The policy file is malformed |
| `POLICY_TARGET_NOT_ALLOWED` | A target resolved to a public address that is not allowlisted, or matched `deny_targets`. Fix: add it to `allow_targets` in the policy file |
| `POLICY_WRITES_NOT_ALLOWED` | A statement or command classified as a write, without `allow_writes` |
| `POLICY_DANGEROUS_NOT_ALLOWED` | A destructive or administrative statement, without `allow_dangerous` |
| `POLICY_RATE_EXCEEDED` | Offered rate above `max_rate_per_runner` |
| `POLICY_DURATION_EXCEEDED` | `run.duration` above `max_duration` |
| `POLICY_INSECURE_TLS_NOT_ALLOWED` | `insecure_skip_verify` without `allow_insecure_tls` |
| `POLICY_TOO_MANY_RUNS` | `max_concurrent_runs` reached. Two tests against one target measure each other |
| `POLICY_DETACH_NOT_ALLOWED` | Background runs are disabled by policy |

### Preflight — exit 3

Preflight runs before any load, so these cost nothing but time.

| Code | Meaning and fix |
| --- | --- |
| `PREFLIGHT_DNS` | A target host does not resolve |
| `PREFLIGHT_CONNECT` | TCP connect failed |
| `PREFLIGHT_HTTP` | The single probe request for a label failed |
| `PREFLIGHT_DB_PING` | The database did not answer |
| `PREFLIGHT_DB_PREPARE` | A statement failed to prepare — a typo, caught without executing it |
| `PREFLIGHT_REDIS_PING` | Redis did not answer |
| `PREFLIGHT_DRIVER_UNKNOWN` | The driver is not compiled in. The message lists the ones that are |

### Run — exit 3, or 4 when invalid

| Code | Exit | Meaning |
| --- | --- | --- |
| `RUN_ABORTED` | 3 | The abort guard stopped the run: errors above 50% for 10s. A partial result is written |
| `RUN_INTERRUPTED` | 3 | A signal or `stop` ended the run early. A partial result is written and marked |
| `RUN_FAILED` | 3 | The run could not produce usable data |
| `RUN_INVALID` | 4 | Validity is `invalid`. The findings say which rule fired and how to fix it. `--allow-invalid` downgrades this to a warning |
| `SLO_BREACH` | 1 | A configured budget was exceeded. This is a successful measurement of a failing target |

### Results, comparison and the run store

| Code | Exit | Meaning |
| --- | --- | --- |
| `RESULT_NOT_FOUND` | 2 | No such result file or run id |
| `RESULT_PARSE` | 2 | The file is not a valid result document |
| `RESULT_SCHEMA_UNSUPPORTED` | 3 | A schema major version with no migration. Names both versions |
| `COMPARE_SCHEMA_MISMATCH` | 3 | Two results whose schema majors cannot be reconciled |
| `COMPARE_INSUFFICIENT_DATA` | 1 | A quantile confidence interval could not be formed — too few samples to call a change real |
| `COMPARE_REGRESSION` | 1 | A configured gate was crossed. `details` names which |
| `RUN_NOT_FOUND` | 2 | No such run in the run store |
| `RUN_ALREADY_RUNNING` | 2 | Another run is active and `max_concurrent_runs` is reached |
| `RUN_LOST` | 3 | A stale heartbeat with a dead pid: the process died without writing a result |
| `WAIT_TIMEOUT` | 5 | `wait --timeout` expired. **The run is still going** — poll again |

### Servers and operations

| Code | Exit | Meaning |
| --- | --- | --- |
| `OPS_UNKNOWN_OPERATION` | 2 | No such operation. `capabilities` lists them |
| `OPS_INVALID_INPUT` | 2 | Input failed the operation's schema. Carries the JSON Pointer |
| `SERVER_UNAUTHORIZED` | 2 | Missing or wrong bearer token |
| `SERVER_BAD_HOST` | 2 | `Host` header rejected — DNS-rebinding protection |
| `SERVER_BAD_ORIGIN` | 2 | `Origin` header rejected — localhost CSRF protection |
| `SERVER_ADDR_IN_USE` | 3 | The listen address is taken |
| `IO_WRITE_FAILED` / `IO_READ_FAILED` | 3 | A filesystem operation failed. Retriable |
| `INTERNAL` | 3 | A bug. Please report it with the run id |

## Findings

Findings are not failures: they appear in `analysis.validity.findings`, in
`warnings`, and as `warning` events. Severity `error` makes a run **invalid**;
`warn` makes it **degraded**; `info` is context. See [ADR-005](adr/005-correlation-methodology.md).

| Code | Severity | Meaning and fix |
| --- | --- | --- |
| `GENERATOR_BEHIND` | error | Dispatch lag p99 exceeded `max(5ms, 5% of a bucket)`: the generator missed its own schedule, so every latency includes our delay. Lower the rate, raise `max_in_flight`, or use a bigger machine |
| `CLIENT_CAPPED` | error | Dropped arrivals above 1% while service time stayed flat — a client-side ceiling, not a target limit. Raise `max_in_flight`; the digest computes the Little's Law figure |
| `TARGET_SATURATED` | warn | Dropped arrivals **with rising** service time. This one is a real finding about the target: it could not keep up |
| `SAME_HOST_TARGET` | warn | The target resolved to loopback, so generator and target shared a machine's CPU. Results are indicative, not a capacity number |
| `GENERATOR_STALL` | warn | In the listed buckets every runner's requests left late at once: the generator's process was paused, typically by a virtualised host. Those buckets are left out of the analysis. Run the generator on a host with dedicated CPU |
| `RATE_LIMITED` | warn | Over 5% of responses were 429: you are measuring the rate limiter, not the service |
| `INSECURE_TLS` | warn | Certificate verification was disabled. Printed in every report |
| `TELEMETRY_UNAVAILABLE` | warn | A sampler could not run. The reason names the grant that would fix it — for example `GRANT pg_read_all_stats` |
| `LITTLES_LAW_INCONSISTENT` | warn | Measured concurrency and `rate x service time` disagree by more than 20%, which usually means an undersized pool or an unnoticed queue |
| `POOL_UNDERSIZED` | warn | Connection pool waits were a material share of latency. Raise the pool to at least the worker count |
| `MAX_CONNECTIONS_EXCEEDED` | warn | `pool.max_open` is above the server's `max_connections`; the server will refuse connections under load |
| `CONN_REUSE_LOW` | warn | Keep-alive is on but connections were not reused — usually `max_idle_conns_per_host` below `max_in_flight` |
| `DANGEROUS_COMMAND` | warn | An O(N) blocking command such as `KEYS` is in the mix; it will distort both the target and the measurement |
| `PROBE_TRIVIAL` | info | Every database probe query is `SELECT 1`: it sees the connection and the server but none of the application's tables, so a healthy probe does not clear the database. Probe with a query the application runs |
| `CAPACITY_INCOMPLETE` | warn | A capacity search stopped before it finished: its time budget (`run.duration`) ran out, the run was stopped, or more than half the operations at a level failed. The boundary reported is what was found so far |
| `CAPACITY_BUDGET_CLAMPED` | info | A capacity search could take longer than the policy's `max_duration`, and no `run.duration` was given, so the policy's ceiling became its budget. It stops there if it has not finished |
| `LABEL_OVERFLOW` | info | More than 50 labels for a runner; the remainder are collected under `other` |
| `LABEL_TIMELINE_DROPPED` | info | Per-label timelines exceeded the retention budget and were dropped. Charts lose per-label detail; no analysis is affected ([ADR-002](adr/002-sketches-and-memory.md)) |
| `INSUFFICIENT_SAMPLES` | info | Buckets fell below `min_samples`. They are drawn but never used as evidence |

### Comparison findings

A comparison's `warnings`, from `tracepoint compare` and `compare_runs`.

| Code | Severity | Meaning and fix |
| --- | --- | --- |
| `COMPARE_INVALID_INPUT` | error | One of the runs is invalid: its numbers describe the generator, so a difference may be the generator's. Re-run it until it is valid |
| `COMPARE_CONFIG_DIFFERS` | warn | The runs were configured differently; the differing keys are listed. A change may come from the configuration rather than the system |
| `COMPARE_RUNNER_MISSING` | warn | A runner is in only one of the runs, so it is not compared |
| `COMPARE_TAIL_ONLY` | info | p99 crossed the relative gate while p50 and p95 stayed within it: only the tail moved. That can be real - contention hitting a few percent of requests - and it is also what a paused host looks like. If nothing in the system changed, re-run both on a quiet host before acting on it |
