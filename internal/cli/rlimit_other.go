//go:build !unix

package cli

// openFileLimit has no portable equivalent on this platform.
func openFileLimit() (int, bool) { return 0, false }
