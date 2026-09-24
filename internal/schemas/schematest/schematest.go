// Package schematest validates emitted documents against the embedded contracts. It is
// imported only by tests, so the validator it wraps never reaches the binary (ADR-007
// scopes that dependency to tests).
package schematest

import (
	"bytes"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/IshaanNene/Tracepoint/internal/schemas"
)

// Compile loads one embedded contract.
func Compile(t testing.TB, name schemas.Name) *jsonschema.Schema {
	t.Helper()
	raw, err := schemas.Get(name)
	if err != nil {
		t.Fatalf("schema %s: %v", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the %s schema is not valid JSON: %v", name, err)
	}
	c := jsonschema.NewCompiler()
	url := "mem://" + string(name) + ".json"
	if addErr := c.AddResource(url, doc); addErr != nil {
		t.Fatalf("adding the %s schema: %v", name, addErr)
	}
	s, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compiling the %s schema: %v", name, err)
	}
	return s
}

// Validate fails the test unless doc satisfies the named contract.
func Validate(t testing.TB, name schemas.Name, doc []byte) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("the %s document is not valid JSON: %v", name, err)
	}
	if err := Compile(t, name).Validate(v); err != nil {
		t.Fatalf("the %s document does not satisfy its schema:\n%v\n%s", name, err, truncate(doc))
	}
}

func truncate(b []byte) string {
	const limit = 4000
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}
