package tracepoint

// Functional options for Run, Compare and Digest live here (WithEvents, WithPolicy,
// WithClock, WithLogger, WithFS, ...). They are added in phase 8 alongside the facade
// in tracepoint.go; keeping them in their own file makes the public surface easy to
// review in a diff and easy for apidiff to track.
