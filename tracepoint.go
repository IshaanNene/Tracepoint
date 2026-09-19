// Package tracepoint is the public, semver-stable Go API for TracePoint: correlated
// HTTP, SQL and Redis load testing that reports which tier a slowdown came from.
//
// Every adapter TracePoint ships - the CLI, the MCP server, the REST server and the
// GitHub Action - reaches the engine only through this package. The surface is
// intentionally small: LoadConfig, Plan, Run, Compare and Digest, configured with
// functional options. See docs/adr/009 for the stability policy and docs/SPEC.md for
// the behaviour it exposes.
//
// The API is defined in phase 8; this file currently carries only the package
// documentation so that the module layout and the stability contract exist from the
// start.
package tracepoint
