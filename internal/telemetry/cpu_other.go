//go:build !unix

package telemetry

import "time"

// processCPU is not available on this platform; cpu_ratio is simply omitted.
func processCPU() (time.Duration, bool) { return 0, false }
