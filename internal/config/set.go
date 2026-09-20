package config

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// ApplySet applies `--set path=value` overrides, in order.
//
// Overrides sit at the top of the precedence chain (§4) and are what closes the agent
// loop: a digest recommends `http.executor.max_in_flight=240`, and
// `tracepoint run --from <run> --set http.executor.max_in_flight=240` applies it without
// anyone reconstructing a configuration from prose.
//
// A path that does not exist is an error, never a silent no-op. An override that
// quietly does nothing would produce a run that looks like it tested the change and did
// not.
func (c *Config) ApplySet(overrides []string) error {
	var problems []*errs.Error
	for _, raw := range overrides {
		path, value, ok := strings.Cut(raw, "=")
		if !ok {
			problems = append(problems, errs.New(errs.CodeSetParse,
				"--set %q is not path=value", raw).
				WithHint("for example --set http.executor.rate=200"))
			continue
		}
		path = strings.TrimSpace(path)
		if path == "" {
			problems = append(problems, errs.New(errs.CodeSetParse,
				"--set %q has an empty path", raw))
			continue
		}
		if err := setPath(reflect.ValueOf(c).Elem(), path, path, value); err != nil {
			problems = append(problems, err)
		}
	}
	if len(problems) > 0 {
		return summarise(problems)
	}
	return nil
}

// setPath walks one dotted path and assigns the value at its end.
func setPath(v reflect.Value, full, rest, value string) *errs.Error {
	segment, remainder, _ := strings.Cut(rest, ".")
	name, index, hasIndex, err := parseSegment(full, segment)
	if err != nil {
		return err
	}

	// A pointer in the middle of a path is a section that may not exist yet; create it
	// so `--set db.pool.max_open=20` works on a configuration without a pool block.
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			if !v.CanSet() {
				return unknownPath(full, name, nil)
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return unknownPath(full, name, nil)
	}

	fields := yamlFields(v.Type())
	field, ok := fields[name]
	if !ok {
		return unknownPath(full, name, fields)
	}
	target := v.FieldByIndex(field.Index)

	if hasIndex {
		target, err = elementOf(target, full, name, index)
		if err != nil {
			return err
		}
	}

	if remainder == "" {
		return assign(target, full, value)
	}
	return setPath(target, full, remainder, value)
}

// parseSegment splits `name[selector]` into its parts.
func parseSegment(full, segment string) (name, index string, hasIndex bool, err *errs.Error) {
	open := strings.IndexByte(segment, '[')
	if open < 0 {
		return segment, "", false, nil
	}
	if !strings.HasSuffix(segment, "]") {
		return "", "", false, errs.New(errs.CodeSetParse,
			"--set %s has an unclosed [ in %q", full, segment).
			WithHint("address a list item by name, as in db.queries[by-id].weight")
	}
	return segment[:open], segment[open+1 : len(segment)-1], true, nil
}

// elementOf resolves a list selector. Items are addressable by their `name` field,
// which is how a person thinks about them, and by index as a fallback.
func elementOf(list reflect.Value, full, field, selector string) (reflect.Value, *errs.Error) {
	if list.Kind() != reflect.Slice {
		return reflect.Value{}, errs.New(errs.CodeSetUnknownPath,
			"--set %s: %s is not a list", full, field).
			WithHint("drop the [...] selector")
	}
	if n, convErr := strconv.Atoi(selector); convErr == nil {
		if n < 0 || n >= list.Len() {
			return reflect.Value{}, errs.New(errs.CodeSetIndexNotFound,
				"--set %s: %s has %d items, so index %d does not exist", full, field, list.Len(), n)
		}
		return list.Index(n), nil
	}

	var names []string
	for i := range list.Len() {
		item := list.Index(i)
		for item.Kind() == reflect.Pointer && !item.IsNil() {
			item = item.Elem()
		}
		if item.Kind() != reflect.Struct {
			continue
		}
		nameField := item.FieldByName("Name")
		if !nameField.IsValid() || nameField.Kind() != reflect.String {
			continue
		}
		if nameField.String() == selector {
			return item, nil
		}
		names = append(names, nameField.String())
	}
	sort.Strings(names)
	e := errs.New(errs.CodeSetIndexNotFound,
		"--set %s: no item named %q in %s", full, selector, field)
	if len(names) > 0 {
		e = e.WithHint("available: %s", strings.Join(names, ", "))
	}
	return reflect.Value{}, e
}

func unknownPath(full, name string, fields map[string]reflect.StructField) *errs.Error {
	e := errs.New(errs.CodeSetUnknownPath, "--set %s: no field %q", full, name)
	if len(fields) == 0 {
		return e
	}
	if suggestion, ok := suggest(name, fields); ok {
		return e.WithHint("did you mean %q?", suggestion)
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	return e.WithHint("available here: %s", strings.Join(names, ", "))
}

// assign converts a string to the target field's type.
//
// The conversion is driven by the destination rather than by guessing at the literal,
// which is what makes `--set run.duration=90s` and `--set http.executor.rate=90` both
// mean what they look like.
func assign(target reflect.Value, full, value string) *errs.Error {
	// A pointer field distinguishes "unset" from "zero", so an override has to
	// allocate before writing.
	for target.Kind() == reflect.Pointer {
		if target.IsNil() {
			if !target.CanSet() {
				return errs.New(errs.CodeSetUnknownPath, "--set %s: that field cannot be set", full)
			}
			target.Set(reflect.New(target.Type().Elem()))
		}
		target = target.Elem()
	}
	if !target.CanSet() {
		return errs.New(errs.CodeSetUnknownPath, "--set %s: that field cannot be set", full)
	}

	// Duration is a named integer type but reads as a string, so it is handled before
	// the generic integer case.
	if target.Type() == reflect.TypeOf(Duration(0)) {
		d, err := time.ParseDuration(value)
		if err != nil {
			return typeMismatch(full, value, "a duration such as 250ms, 30s or 3m")
		}
		target.Set(reflect.ValueOf(Duration(d)))
		return nil
	}

	switch target.Kind() {
	case reflect.String:
		target.SetString(value)
	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return typeMismatch(full, value, "true or false")
		}
		target.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return typeMismatch(full, value, "a whole number")
		}
		if target.OverflowInt(n) {
			return typeMismatch(full, value, "a whole number this field can hold")
		}
		target.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return typeMismatch(full, value, "a non-negative whole number")
		}
		if target.OverflowUint(n) {
			return typeMismatch(full, value, "a number this field can hold")
		}
		target.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return typeMismatch(full, value, "a number")
		}
		target.SetFloat(f)
	case reflect.Slice:
		return assignSlice(target, full, value)
	case reflect.Map:
		return assignMapEntry(target, full, value)
	default:
		return errs.New(errs.CodeSetTypeMismatch,
			"--set %s: %s cannot be set from the command line", full, target.Kind()).
			WithHint("change it in the configuration file instead")
	}
	return nil
}

// assignSlice replaces a list from a comma-separated value, which is the only shape
// that reads naturally on a command line.
func assignSlice(target reflect.Value, full, value string) *errs.Error {
	elem := target.Type().Elem()
	if elem.Kind() != reflect.String && elem.Kind() != reflect.Int {
		return errs.New(errs.CodeSetTypeMismatch,
			"--set %s: a list of %s cannot be set from the command line", full, elem.Kind()).
			WithHint("change it in the configuration file instead")
	}
	if strings.TrimSpace(value) == "" {
		target.Set(reflect.MakeSlice(target.Type(), 0, 0))
		return nil
	}
	parts := strings.Split(value, ",")
	out := reflect.MakeSlice(target.Type(), 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch elem.Kind() {
		case reflect.String:
			out = reflect.Append(out, reflect.ValueOf(p).Convert(elem))
		case reflect.Int:
			n, err := strconv.Atoi(p)
			if err != nil {
				return typeMismatch(full, p, "a whole number")
			}
			out = reflect.Append(out, reflect.ValueOf(n).Convert(elem))
		default:
		}
	}
	target.Set(out)
	return nil
}

// assignMapEntry sets one key of a string map, written as key:value.
func assignMapEntry(target reflect.Value, full, value string) *errs.Error {
	if target.Type().Key().Kind() != reflect.String || target.Type().Elem().Kind() != reflect.String {
		return errs.New(errs.CodeSetTypeMismatch,
			"--set %s: that map cannot be set from the command line", full)
	}
	key, val, ok := strings.Cut(value, ":")
	if !ok {
		return typeMismatch(full, value, "key:value")
	}
	if target.IsNil() {
		target.Set(reflect.MakeMap(target.Type()))
	}
	target.SetMapIndex(reflect.ValueOf(strings.TrimSpace(key)), reflect.ValueOf(strings.TrimSpace(val)))
	return nil
}

func typeMismatch(full, value, want string) *errs.Error {
	return errs.New(errs.CodeSetTypeMismatch,
		"--set %s: %q is not %s", full, value, want).
		WithHint("expected %s", want)
}

// SetPaths lists every path --set understands, for `--help` and the capabilities
// manifest. Generated from the types, so it cannot drift from what actually works.
func SetPaths() []string {
	var out []string
	collectPaths(reflect.TypeOf(Config{}), "", &out, 0)
	sort.Strings(out)
	return out
}

func collectPaths(t reflect.Type, prefix string, out *[]string, depth int) {
	const maxDepth = 6
	if depth > maxDepth {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || implementsUnmarshaler(t) {
		return
	}
	for name, field := range yamlFields(t) {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Struct:
			if implementsUnmarshaler(ft) || ft == reflect.TypeOf(time.Time{}) {
				*out = append(*out, path)
				continue
			}
			collectPaths(ft, path, out, depth+1)
		case reflect.Slice:
			*out = append(*out, path+"[name]")
			collectPaths(ft.Elem(), path+"[name]", out, depth+1)
		default:
			*out = append(*out, path)
		}
	}
}
