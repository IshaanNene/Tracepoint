# ADR-007: Which third-party modules are allowed, and why

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §2 (dependencies)

## Context

Every dependency is a supply-chain surface, a licence obligation, a potential CGO
requirement that would break the static-binary promise, and something that can be
abandoned. It is also, often, thousands of lines we would otherwise write and get
wrong. The rule is stdlib first, and a justification for everything else.

## Decision

**Admission criteria**, all four required: actively maintained; MIT, BSD or Apache-2.0
licensed; no CGO in the configuration we use; and a job the standard library does not
already do well.

**Dependencies are added in the phase that needs them, not up front.** Phase 0 ships
with a `go.mod` that has zero third-party requirements, so the skeleton's `make check`
proves the toolchain rather than the module cache.

| Module | Purpose | Why not stdlib | Licence |
| --- | --- | --- | --- |
| `spf13/cobra` | Command tree | `flag` gives no subcommands, no help examples and no introspectable command tree — and `capabilities` (§6.1) is *generated* from that tree, so introspection is a requirement, not a nicety | Apache-2.0 |
| `go.yaml.in/yaml/v3` | YAML with strict decoding and line/column errors | No YAML in stdlib. The maintained successor to the archived `gopkg.in/yaml.v3`; `KnownFields(true)` plus positional errors is what makes §4's "unknown key with a did-you-mean hint" possible | Apache-2.0 |
| `jackc/pgx/v5` (stdlib mode) | Postgres driver | Used via `database/sql` so the pool accounting is uniform across drivers. Pure Go; the de facto Postgres driver | MIT |
| `go-sql-driver/mysql` | MySQL driver | Pure Go, canonical | MPL-2.0 (file-level copyleft; used unmodified as a library, which imposes nothing on this project) |
| `modernc.org/sqlite` | SQLite driver | The reason release builds can be `CGO_ENABLED=0` at all. `mattn/go-sqlite3` would force CGO and break cross-compilation to six targets | BSD-3-Clause |
| `redis/go-redis/v9` | Redis client | `UniversalClient` covers single, cluster and sentinel behind one type, with pool statistics we surface | BSD-2-Clause |
| `tidwall/gjson` | JSON path extraction | Reads a path without unmarshalling the whole body — on the hot path, for every journey step | MIT |
| `DataDog/sketches-go` | DDSketch | The reference implementation of the algorithm in ADR-002, with the relative-error guarantee we depend on | Apache-2.0 |
| `google.golang.org/protobuf` | — (indirect) | Not chosen: `sketches-go`'s `ddsketch` package imports its own protobuf bindings, so this arrives transitively even though TracePoint uses the library's non-proto binary codec. Recorded here so nothing in `go.mod` is unexplained | BSD-3-Clause |
| `charmbracelet/bubbletea` + `lipgloss` | Live terminal view | TTY-only, and the only dependency the core can run without. Pinned at bubbletea v1.3.10 and lipgloss v1.1.0 (phase 5): v2 now lives at `charm.land/bubbletea/v2`, and moving is a later, deliberate change. `muesli/termenv`, already beneath lipgloss, is imported directly only to force a colourless profile when colour is off | MIT |
| `modelcontextprotocol/go-sdk` | MCP server | The official SDK. Writing a protocol implementation by hand guarantees drift from a spec that is still moving | MIT, relicensing to Apache-2.0 (verified at v1.8.0: new contributions are Apache-2.0, older ones remain MIT; both permissive) |
| `google/jsonschema-go` | Operation input and output schemas | Arrives with the MCP SDK, which uses it for tool schemas. Deriving each operation's schemas from its handler's Go types with it, and validating input against exactly the schema the agent was shown, is what keeps the three surfaces from drifting (added in phase 4) | MIT |
| `santhosh-tekuri/jsonschema/v6` | JSON Schema validation in tests | Validates every emitted document against its own schema (§6.7). 2020-12 support; test-scope | Apache-2.0 |

The MCP SDK brings, indirectly: `yosida95/uritemplate/v3` (resource templates),
`segmentio/encoding` and `segmentio/asm` (its JSON codec), `golang.org/x/oauth2`
(its optional auth support, unused here) and `golang.org/x/time`. None is imported
directly; they are listed so nothing in `go.mod` is unexplained.

Licences in the table are the modules' stated licences at the time of this record.
Each is re-verified when the dependency is actually added, and the SBOM plus a
licence check in CI (§11) is the standing enforcement — this table is the intent,
the SBOM is the evidence.

**Test-only**: `testcontainers-go` (real Postgres, MySQL and Redis for integration
tests), `alicebob/miniredis` (fast in-process Redis for unit tests),
`go.uber.org/goleak` (goroutine-leak detection in every package's `TestMain`),
`pgregory.net/rapid` (property tests for the scheduler, sketches and incident
merging).

**Vendored, not imported**: the HTML report's chart library, embedded with `go:embed`
and keeping its `LICENSE` file, because the report must work offline with zero network
requests (ADR-006, threat 8). Chosen in phase 5: uPlot 1.6.32 (MIT) - about 50 KB,
built for dense time series, draws on canvas, and styles through the CSSOM, so it runs
under a CSP that allows no inline style attributes. Its provenance, integrity and
SHA-256 are in `internal/render/html/assets/uplot/PROVENANCE.md`, and a test pins the
hashes.

### Explicitly rejected

- **vegeta, k6's engine, or any load-generation library** — forbidden by §2, and the
  scheduler is the core intellectual property (ADR-001).
- **A logging library** — `log/slog` is in the standard library and does structured
  output natively.
- **An HTTP router for the REST adapter** — `net/http`'s `ServeMux` has handled method
  and pattern matching since Go 1.22, and the adapter has a dozen routes generated
  from the ops registry.
- **A CSV library** — `encoding/csv` is sufficient for feeders.
- **`mattn/go-sqlite3`** — CGO, which would cost the static binary and the
  cross-compilation matrix.

## Consequences

- Release builds are `CGO_ENABLED=0` and cross-compile to all six targets; `-race`
  test runs enable cgo, which is the only place a C toolchain is needed.
- MySQL's MPL-2.0 is the one non-permissive licence in the set. It is file-level
  copyleft and the module is used unmodified as a library, so it imposes no obligation
  on this project; it is recorded here so the choice is deliberate rather than
  accidental, and it is listed in the SBOM.
- `govulncheck` runs in `make check` and in CI; Dependabot or Renovate keeps versions
  current.
- Adding a dependency later means amending this record. That friction is intentional.
