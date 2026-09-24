//go:build !unix

package runstore

// processAlive cannot be checked portably here, so a stale heartbeat alone marks a run
// lost: StaleAfter is several heartbeats long, and a live run always beats.
func processAlive(int) bool { return false }
