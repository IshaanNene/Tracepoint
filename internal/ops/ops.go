// Package ops is the one registry of agent operations (§6.5, ADR-008). Each operation
// is defined once - a name, a description written for a model, input and output JSON
// Schemas, annotations and a handler - and the MCP tools, the REST endpoints, the
// OpenAPI document and the capabilities manifest are all generated from it. A parity
// test holds them together, so the surfaces cannot drift from each other or from the
// CLI.
//
// Input and output schemas are derived from the handler's Go types, so the schema an
// agent reads and the decoding the handler performs cannot disagree either.
package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Annotations describe an operation's effects, for clients that gate on them.
type Annotations struct {
	ReadOnly    bool `json:"read_only"`
	Idempotent  bool `json:"idempotent"`
	Destructive bool `json:"destructive"`
	// OpenWorld is true when the operation reaches outside TracePoint: it contacts
	// targets.
	OpenWorld bool `json:"open_world"`
}

// Operation is one agent operation.
type Operation struct {
	Name        string
	Title       string
	Description string
	Annotations Annotations
	// CLI is the equivalent command, for the parity test and for agents that have a
	// shell. Empty when an operation exists only for servers.
	CLI string

	Input  *jsonschema.Schema
	Output *jsonschema.Schema

	inType  reflect.Type
	input   *jsonschema.Resolved
	handler func(ctx context.Context, s *Service, in any) (any, error)
}

// define builds an operation from a typed handler, deriving both schemas.
func define[In, Out any](op Operation, h func(context.Context, *Service, *In) (*Out, error), shape func(in, out *jsonschema.Schema)) Operation {
	in, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("ops: input schema for %s: %v", op.Name, err)) // a programming error, caught by every test
	}
	out, err := jsonschema.For[Out](&jsonschema.ForOptions{IgnoreInvalidTypes: true})
	if err != nil {
		panic(fmt.Sprintf("ops: output schema for %s: %v", op.Name, err))
	}
	// Unknown input fields are refused: a misspelled argument that is silently
	// ignored produces a call that looks right and does something else.
	in.AdditionalProperties = &jsonschema.Schema{Not: &jsonschema.Schema{}}
	if shape != nil {
		shape(in, out)
	}
	resolved, err := in.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("ops: resolving the input schema for %s: %v", op.Name, err))
	}
	op.Input, op.Output, op.input = in, out, resolved
	op.inType = reflect.TypeFor[In]()
	op.handler = func(ctx context.Context, s *Service, v any) (any, error) {
		typed, ok := v.(*In)
		if !ok {
			return nil, errs.New(errs.CodeInternal, "ops: %s received %T", op.Name, v)
		}
		return h(ctx, s, typed)
	}
	return op
}

// Invoke validates raw input against the operation's schema, decodes it strictly and
// runs the handler.
func (op Operation) Invoke(ctx context.Context, s *Service, raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage("{}")
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, errs.Wrap(errs.CodeOpsInvalidInput, err, "the input to %s is not JSON", op.Name)
	}
	if err := op.input.Validate(generic); err != nil {
		return nil, errs.New(errs.CodeOpsInvalidInput, "the input to %s does not match its schema: %s",
			op.Name, errs.CleanUntrusted(err.Error())).
			WithHint("the schema is in the tool definition and in `tracepoint capabilities`")
	}
	in := reflect.New(op.inType).Interface()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		return nil, errs.Wrap(errs.CodeOpsInvalidInput, err, "decoding the input to %s", op.Name)
	}
	return op.handler(ctx, s, in)
}

// Registry is every operation, in a stable order. It is built once per call so that
// callers never share mutable schema values.
func Registry() []Operation {
	all := []Operation{
		getPolicy(), scaffoldConfig(), validateConfig(), planRun(), startRun(),
		getRunStatus(), waitForRun(), stopRun(), getRunDigest(), getRunSection(), listRuns(),
		renderReport(), compareRuns(),
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all
}

// Find looks an operation up by name.
func Find(name string) (Operation, error) {
	for _, op := range Registry() {
		if op.Name == name {
			return op, nil
		}
	}
	names := make([]string, 0, 16)
	for _, op := range Registry() {
		names = append(names, op.Name)
	}
	return Operation{}, errs.New(errs.CodeOpsUnknownOperation, "no operation named %q", name).
		WithHint("available: %s", strings.Join(names, ", "))
}

// Summary is a one-line, human-readable account of an operation's result, for MCP
// clients that do not render structured content.
func Summary(op Operation, out any, err error) string {
	if err != nil {
		var typed *errs.Error
		if errors.As(err, &typed) {
			s := fmt.Sprintf("%s failed: %s: %s", op.Name, typed.Code, typed.Message)
			if typed.Hint != "" {
				s += " (" + typed.Hint + ")"
			}
			return s
		}
		return fmt.Sprintf("%s failed: %v", op.Name, err)
	}
	if s, ok := out.(interface{ summary() string }); ok {
		return s.summary()
	}
	return op.Name + " succeeded"
}

func ptr[T any](v T) *T { return &v }
