//go:build !unix

package result

// openFileLimit has no portable equivalent on this platform.
func openFileLimit() (int, bool) { return 0, false }
