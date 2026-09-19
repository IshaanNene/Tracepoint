# Contributing

Thanks for looking. A few things make contributions land smoothly here.

## Read first

[`docs/SPEC.md`](docs/SPEC.md) is the source of truth and is stored verbatim — it is
never edited. Where it is silent, the decision lives in [`docs/adr/`](docs/adr/).
[`docs/PROGRESS.md`](docs/PROGRESS.md) says what is actually built.

If you are a coding agent, [`CLAUDE.md`](CLAUDE.md) is written for you.

## The gate

```bash
make check
```

Format, vet, lint, race tests, contract checks and `govulncheck`. It must be green
before you open a pull request. `make help` lists everything else.

## Conventions worth knowing before you write code

These are from §2 of the spec, and breaking one is a bug rather than a style
disagreement:

- **`result.json` is the system of record.** Every renderer is a pure function of it.
  A number that is not in `internal/schemas/result.schema.json` cannot appear in any output, so
  a new statistic starts as a schema change.
- **Never average percentiles.** Merge sketches and read the merged result.
- **Nothing reads `time.Now`** — take a `clock.Clock`. Nothing reads a global RNG —
  take a seeded one. No mutable package-level state; a run must be reproducible from
  its recorded seed.
- **`context.Context` on every blocking call.** `goleak` runs in every package's
  `TestMain`, so a leaked goroutine fails the build.
- **Errors wrap with `%w`** and carry a stable code from [`docs/ERRORS.md`](docs/ERRORS.md).
  No panics outside `main`.
- **The math packages are written test-first**: `schedule`, `metrics`, `analysis`,
  `capacity`, `compare`. They also carry an 85% coverage floor.
- **No feature is done until its agent interface is done.** A capability reachable
  only from an interactive terminal is unfinished.

## Tests

Table-driven, with a fake clock wherever timing matters. Property tests use
`pgregory.net/rapid`; golden files are updated with `go test -update`. Tests that need
containers are behind build tags: `make integration` and `make e2e` (both need
Docker), `make soak` for the ten-minute flat-heap run.

## Commits and pull requests

Conventional commits, kept small: `feat(schedule): invert Lambda in closed form`,
`fix(metrics): seal buckets after the max timeout`, `docs(adr): record the sketch
choice`.

A pull request should say what changed, what you ran to verify it, and anything you
deliberately left out. If you found a real problem with the approach, say so — but
please finish the piece of work and flag the concern rather than narrowing the scope
quietly.

## Adding a dependency

Amend [ADR-007](docs/adr/007-dependencies.md) in the same pull request with what it
does, why the standard library will not, its licence and its maintenance status. The
friction is intentional.

## Licence

Contributions are accepted under the MIT licence in [`LICENSE`](LICENSE).
