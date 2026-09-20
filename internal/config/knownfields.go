package config

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// checkKnownFields walks a parsed document against the configuration's own types and
// reports every key that does not belong, with its line, its column and a suggestion.
//
// The YAML decoder can reject unknown fields on its own, but it stops at the first one
// and its message carries no position or suggestion. A configuration with three typos
// should report three typos, each pointing at where it is - a load test is slow enough
// that finding out about the second mistake on the second run is a real cost.
func checkKnownFields(doc *yaml.Node, t reflect.Type) []*errs.Error {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	var found []*errs.Error
	walk(root, t, "", &found)
	return found
}

func walk(n *yaml.Node, t reflect.Type, path string, out *[]*errs.Error) {
	if n == nil {
		return
	}
	// An alias node points at content already checked where it was defined.
	if n.Kind == yaml.AliasNode {
		return
	}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return
	}

	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return
		}
		// A type that decodes itself - Duration, Sampler - owns its own contents.
		if implementsUnmarshaler(t) {
			return
		}
		known := yamlFields(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			name := key.Value
			field, ok := known[name]
			if !ok {
				*out = append(*out, unknownField(name, path, key, known))
				continue
			}
			walk(value, field.Type, path+"/"+name, out)
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != yaml.SequenceNode {
			return
		}
		for i, item := range n.Content {
			walk(item, t.Elem(), path+"/"+strconv.Itoa(i), out)
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return
		}
		// Map keys are data, not field names, so only the values are checked.
		for i := 0; i+1 < len(n.Content); i += 2 {
			walk(n.Content[i+1], t.Elem(), path+"/"+n.Content[i].Value, out)
		}
	case reflect.Interface:
		// `any` accepts whatever it is given: a bind argument, a JSON literal.
	default:
	}
}

func unknownField(name, path string, key *yaml.Node, known map[string]reflect.StructField) *errs.Error {
	e := errs.New(errs.CodeConfigUnknownField, "unknown field %q", name).
		WithPath(path+"/"+name).
		WithPos(key.Line, key.Column)
	if suggestion, ok := suggest(name, known); ok {
		return e.WithHint("did you mean %q?", suggestion)
	}
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return e.WithHint("valid fields here: %s", strings.Join(names, ", "))
	}
	return e
}

// suggest finds the closest known field, if one is close enough to be worth offering.
// The threshold scales with the length of what was typed: "metod" for "method" is
// worth suggesting, "x" for "extract" is not.
func suggest(name string, known map[string]reflect.StructField) (string, bool) {
	limit := len(name)/3 + 1
	best, bestDist := "", limit+1
	for candidate := range known {
		if d := editDistance(strings.ToLower(name), strings.ToLower(candidate)); d < bestDist {
			best, bestDist = candidate, d
		}
	}
	if bestDist <= limit {
		return best, true
	}
	return "", false
}

// editDistance is Levenshtein distance over two rows rather than a full matrix.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// yamlFields maps a struct's YAML key names to its fields, following embedded structs.
func yamlFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		switch {
		case name == "-":
			continue
		case name == "" && f.Anonymous:
			for k, v := range yamlFields(f.Type) {
				out[k] = v
			}
			continue
		case name == "":
			name = strings.ToLower(f.Name)
		}
		out[name] = f
	}
	return out
}

//nolint:gochecknoglobals // A reflect.Type constant; computing it once is the point.
var unmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()

func implementsUnmarshaler(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(unmarshalerType)
}
