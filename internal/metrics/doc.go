// Package metrics records outcomes on the hot path and summarises them.
//
// Per (runner, label, bucket) it keeps a relative-error sketch and per-class counters in
// per-worker shards, seals buckets into fixed-size summaries, and merges sketches for
// whole-run quantiles. Memory is independent of run length. See docs/adr/002.
package metrics
