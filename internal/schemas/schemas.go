// Package schemas embeds the versioned JSON Schemas.
//
// They live inside the package rather than at the repository root so that go:embed can
// reach them, which means the binary always ships exactly the contracts it honours:
// `tracepoint schema config` cannot emit a schema that disagrees with the code that
// enforces it, because there is only one copy.
package schemas

import (
	"embed"
	"sort"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

//go:embed *.schema.json
var files embed.FS

// Name identifies one contract document.
type Name string

// The contracts, in the order `tracepoint schema` lists them.
const (
	Config Name = "config"
	Policy Name = "policy"
	Result Name = "result"
	Digest Name = "digest"
	Events Name = "events"
	Error  Name = "error"
)

//nolint:gochecknoglobals // A fixed table, never mutated after init.
var descriptions = map[Name]string{
	Config: "a TracePoint run configuration",
	Policy: "the safety envelope a human grants",
	Result: "result.json, the system of record for one run",
	Digest: "the prioritised, context-sized summary an agent reads",
	Events: "one line of the append-only event stream",
	Error:  "the error envelope every failure is reported in",
}

// Names lists every contract, sorted, for `tracepoint schema --help` and for the
// capabilities manifest.
func Names() []Name {
	out := make([]Name, 0, len(descriptions))
	for n := range descriptions {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Describe returns a one-line description of a contract.
func Describe(n Name) string { return descriptions[n] }

// Get returns a schema document.
func Get(n Name) ([]byte, error) {
	b, err := files.ReadFile(string(n) + ".schema.json")
	if err != nil {
		return nil, errs.New(errs.CodeOpsInvalidInput, "no schema named %q", n).
			WithHint("available schemas: %s", strings.Join(nameStrings(), ", "))
	}
	return b, nil
}

func nameStrings() []string {
	all := Names()
	out := make([]string, 0, len(all))
	for _, n := range all {
		out = append(out, string(n))
	}
	return out
}
