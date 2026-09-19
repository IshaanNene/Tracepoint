# Assumptions and open questions

Phase 0 deliverable (§12.0). Everything here is a judgement the spec did not make for
us. Each assumption has a default already applied, so work is not blocked; each one is
cheap to change now and progressively more expensive later, and the "cost to change"
column says when.

## Assumptions applied

| # | Assumption | Why | Cost to change later |
| --- | --- | --- | --- |
| A1 | The tool is named `tracepoint`; module path `github.com/IshaanNene/Tracepoint` | The spec's title says TracePoint and instructs one rename of the working name `strata`; the repository agrees | High after phase 1 — it is in the module path, the binary, `TRACEPOINT_*`, `X-Tracepoint-Run-Id`, run directories and the skill path, all §6.7 contracts. See [ADR-010](adr/010-name.md) |
| A2 | Config key names are now fixed, as encoded in `schemas/config.schema.json` | §4 says "finalize key names in Phase 0, then keep them consistent". The spec's illustrative config validates against the schema with zero errors | High — it is the config contract |
| A3 | Per-label *timelines* have a retention budget (default 20,000 `labels x buckets` entries); per-runner timelines are always kept | §3.4 wants per-(runner,label,bucket) summaries *and* a flat heap over a ten-minute soak. At 50 labels those two conflict by about 6 MB. No analysis reads per-label timelines — they are for the HTML report — so dropping them past a budget changes presentation, never a verdict | Low — internal, with one warning code and one result field |
| A4 | `go.mod` carries zero third-party modules in phase 0; dependencies arrive in the phase that needs them | Keeps the skeleton's `make check` a test of the toolchain. Each is pre-justified in [ADR-007](adr/007-dependencies.md) | None |
| A5 | Error classes are a fixed, named set (`timeout`, `connection`, `dns`, `tls`, `pool_timeout`, `http_4xx`, `http_5xx`, `expect_failed`, `extract_failed`, `query_error`, `canceled`, `other`) | The abort guard must count 5xx but not 4xx or 429 (§8), and the validity rules must separate generator-side waits from target failures. That requires the taxonomy to be explicit | Medium — readers are told to tolerate unknown keys, so adding one is additive |
| A6 | `capacity` is a config section, not flags only | §5.6 describes the search but gives no config shape, while everything else in the tool is configurable and reproducible from a file | Low |
| A7 | `http.base_url` exists, so steps can be written as paths | Journeys with ten steps would otherwise repeat `${BASE_URL}` ten times | Low — additive |
| A8 | Feeders are declared per runner (`http.feeders`) and referenced as `{{feedername.column}}` | §4 gives the token form `{{feeder.column}}` but no declaration site | Low |
| A9 | A `tx` unit is one item in `db.queries` with `tx:` instead of `sql:`, timed as a unit | §5.1 asks for "optional multi-statement `tx` unit" without a shape | Low |
| A10 | Run directories are created under `./runs/` relative to the working directory | §6.4 specifies the layout as `runs/<UTC timestamp>-<short id>/`. `--out-dir` and `policy.run_root` override it | Low |
| A11 | `requests` items may declare `extract`, even though a single-step journey has no later step to use it | Keeps one step shape after desugaring ([ADR-003](adr/003-executors-and-desugaring.md)); the dataflow check never complains about an unused extraction | Low |
| A12 | `telemetry.<sampler>` accepts a boolean or an object | The spec's own example writes `telemetry: { postgres: true, redis: true }`, and configuring an interval needs the object | Low |

## Open questions

None of these block phases 1–3. They are listed in the order they start to matter.

| # | Question | Default being applied | First phase it binds |
| --- | --- | --- | --- |
| Q1 | Is `tracepoint` the final name (A1)? | Yes, per ADR-010 | 1 — it enters the module path and every contract |
| Q2 | CI matrix Go versions — pin to the one latest stable, or also test the previous minor? | Latest stable only, matching §2. `go.mod` pins `go 1.26.6`, which is the first release with no `govulncheck` findings against the standard library for the packages this tool calls | 1 |
| Q3 | Should `runs/` default somewhere outside the working directory (e.g. an XDG state directory) so running in a repo does not litter it? | `./runs/` per §6.4, `.gitignore`d | 4 — the run store |
| Q4 | Which chart library is vendored into the HTML report? It must be small, offline, permissively licensed, and work under a CSP with no `unsafe-inline` | To be chosen in phase 5 with a dependency-style justification appended to ADR-007 | 5 |
| Q5 | Does `compare` accept results from different tool versions with the same schema major? | Yes, with a warning listing the version difference | 7 |
| Q6 | Release targets beyond the six in §2 — a Homebrew tap, and which container registry? | GoReleaser to GitHub Releases plus GHCR; the tap is optional per §11 | 8 |
| Q7 | Does the faultbox demo need to run without Docker for contributors who lack it? | No — `make e2e` requires Docker, `make check` never does | 3 |

## Environment notes for this machine

- The working tree is under `~/Desktop`, which is iCloud-synced. Build output (`bin/`,
  `dist/`), coverage files and run directories are `.gitignore`d, and the Makefile
  builds into `bin/` rather than the package directories, which keeps synced copies out
  of the tree. Worth remembering if a stale duplicate file ever appears.
- `go1.26.6` (auto-downloaded via the `go` directive) and `golangci-lint 2.12.2` are
  installed, as is `govulncheck`. `goreleaser` is not yet; `make check` reports the exact install command rather than skipping the
  step silently.
