// Package buildinfo carries the version stamped into the binary at link time,
// falling back to the Go module build information for `go install` builds.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Values injected with -ldflags -X at release time. Defaults suit local builds.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Info is the machine-readable build description reported by `tracepoint version
// --output json` and embedded in every result document.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Get returns the build description, preferring link-time values and filling the
// gaps from the embedded module build info.
func Get() Info {
	i := Info{
		Name:    "tracepoint",
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return i
	}
	if i.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		i.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if i.Commit == "none" {
				i.Commit = s.Value
			}
		case "vcs.time":
			if i.Date == "unknown" {
				i.Date = s.Value
			}
		}
	}
	return i
}

// UserAgent is the HTTP User-Agent TracePoint identifies itself with, so that test
// traffic is filterable in the target's own logs and APM.
func UserAgent(runID string) string {
	return "tracepoint/" + Version + " (+run=" + runID + ")"
}
