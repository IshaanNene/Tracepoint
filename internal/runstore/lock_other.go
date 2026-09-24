//go:build !unix

package runstore

import "os"

// Advisory file locks are not portable here, so only starts within one process are
// serialised; the limit between separate processes is checked but not locked.
func lockFile(*os.File) error { return nil }
