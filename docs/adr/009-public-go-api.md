# ADR-009: The public Go API and its stability policy

- Status: Accepted
- Date: 2026-09-19
- Phase: 0
- Spec: §2 (hexagonal), §6.7 (contract stability), §7 (Go library)

## Context

Four adapters — the CLI, the MCP server, the REST server and the GitHub Action — plus
anyone embedding TracePoint in their own Go tests, all need the engine. If adapters
reach into `internal/`, business logic leaks into them and every adapter grows its own
slightly different version of "run a test and decide whether it passed". And a Go API
is a permanent commitment: unlike a CLI flag, a removed exported symbol breaks a build
somewhere with no deprecation window unless one is planned.

## Decision

**A small facade at the module root, and everything else internal.**

```go
package tracepoint

func LoadConfig(ctx context.Context, src Source, opts ...Option) (*Config, error)
func Plan(ctx context.Context, cfg *Config, opts ...Option) (*PlanResult, error)
func Run(ctx context.Context, cfg *Config, opts ...Option) (*Result, error)
func Compare(ctx context.Context, baseline, current *Result, opts ...Option) (*Comparison, error)
func Digest(r *Result, opts ...Option) (*DigestDoc, error)
```

with functional options — `WithEvents(func(Event))`, `WithPolicy(p)`, `WithClock(c)`,
`WithLogger(*slog.Logger)`, `WithRand(seed)`, `WithFS(root)`, `WithOutputDir(dir)` —
and `tracepointtest.Run(t, cfg)`, which fails a Go test on an SLO breach or an invalid
run.

**Every adapter goes through this surface and nothing else.** That is what keeps the
adapters thin enough to be obviously correct, and it means the library is exercised by
every one of our own tools rather than being an afterthought that only external users
hit. Rendering helpers may stay internal, because a renderer is a pure function of
`Result` and re-exporting the whole render tree would freeze far more surface than it
earns.

**Functional options rather than a config struct** because they extend without
breaking: a new option is additive, whereas a new struct field with meaningful
behaviour changes the zero value's meaning for every existing caller.

**Concrete types, not interfaces, for data.** `*Result`, `*Config` and `*DigestDoc`
are structs with JSON tags matching the schemas exactly. Interfaces appear only where
behaviour is injected (`Clock`, the event callback), because an interface returned
from an API is a promise about a method set that is much harder to evolve than a
struct that can gain a field.

**Stability.** SemVer, and within a major version, additive only. A deprecation is
announced in `CHANGELOG`, in `capabilities.deprecations` and as a warning event for at
least one minor version before removal in the next major. `apidiff` (or `gorelease`)
runs in CI against the previous tag, so a breaking change is a failed build rather
than a discovery made by a user.

**Pre-1.0**, the API may change between minor versions; that is stated in the README
and in the package documentation rather than implied.

## Consequences

- Nothing under `internal/` is importable from outside, which is enforced by the
  compiler rather than by convention — a real advantage of doing the layout this way
  from the start.
- The facade is deliberately written late, in phase 8, once the shapes it exposes have
  been proven by the internal packages and the adapters. Publishing an API before
  knowing what it needs to say is how APIs end up with `Options` structs full of
  deprecated fields. `tracepoint.go` and `options.go` exist from phase 0 carrying only
  package documentation, so the contract has a home and `apidiff` has a baseline.
- Two exported names must stay in step with the schemas: `Result` mirrors
  `result.schema.json` and `DigestDoc` mirrors `digest.schema.json`. A test
  round-trips each through its schema, so a struct field and its contract cannot
  diverge.
- `context.Context` is the first parameter of everything that blocks, and cancellation
  is honoured throughout; `goleak` in every package's `TestMain` is what keeps that
  from being aspirational.

## Alternatives considered

- **Export the internal packages.** Every refactor becomes a breaking change, and the
  public surface becomes everything anyone ever imported by accident.
- **A `Runner` struct with exported fields instead of options.** Mutable configuration
  after construction, no validation point, and a zero value that means something
  different in each release.
- **No public API; adapters reach into internals.** The CLI would become the real API
  by default, which is how "just shell out to it" becomes an integration strategy.
