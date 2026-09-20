// Package template compiles the configuration mini-language once, at load time.
//
// A template is a string that may contain {{tokens}}: a variable extracted by an
// earlier journey step, a feeder column, or one of a fixed set of generators. Compiling
// up front rather than parsing per operation keeps the hot path free of parsing work,
// and - more importantly - turns a typo into a configuration error reported before any
// load is generated, instead of a literal "{{randInt 1 10}}" being sent to a target
// thousands of times.
//
// One rule from §4 shapes the whole design: a literal {{token}} must never reach a
// target. A template that cannot be rendered fails the operation with a coded error.
package template

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Generator names. An unknown one is a configuration error, not a variable.
const (
	fnRandInt    = "randInt"
	fnRandString = "randString"
	fnUUID       = "uuid"
	fnSeq        = "seq"
	fnPick       = "pick"
	fnNow        = "now"
)

//nolint:gochecknoglobals // A fixed table, never mutated after init.
var knownFuncs = map[string]struct {
	minArgs, maxArgs int
	usage            string
}{
	fnRandInt:    {2, 2, "randInt <min> <max> - a whole number in the inclusive range"},
	fnRandString: {1, 1, "randString <length> - that many random alphanumeric characters"},
	fnUUID:       {0, 0, "uuid - a random version 4 UUID"},
	fnSeq:        {0, 0, "seq - a counter, unique across the run, starting at 1"},
	fnPick:       {1, 1, "pick <a|b|c> - one of the alternatives"},
	fnNow:        {0, 0, "now - the current time"},
}

// FunctionNames lists the generators, sorted, for error messages and documentation.
func FunctionNames() []string {
	out := make([]string, 0, len(knownFuncs))
	for name := range knownFuncs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// kind distinguishes what a token resolves to.
type kind uint8

const (
	kindLiteral kind = iota
	kindVar          // {{name}} - extracted by an earlier step
	kindFeeder       // {{feeder.column}}
	kindGen          // a generator call
	kindValue        // an already-typed value, wrapped by Literal
)

// part is one compiled piece of a template.
type part struct {
	kind    kind
	literal string
	// value holds an already-typed literal, for kindValue.
	value any
	// name is the variable name, the feeder name, or the function name.
	name   string
	column string   // feeder column
	args   []string // generator arguments, already validated
	// numeric arguments, parsed at compile time so rendering does no conversion
	lo, hi int64
	length int
	picks  []string
}

// Template is a compiled string template.
type Template struct {
	source string
	parts  []part
	// solo is set when the template is exactly one token and nothing else. Such a
	// template keeps its natural type, so {{randInt 1 100}} binds to SQL as an integer
	// rather than as the string "42".
	solo *part
}

// Compile parses a template. It returns an error for anything that could not be
// rendered later: an unclosed token, an unknown generator, a bad argument.
func Compile(source string) (*Template, error) {
	t := &Template{source: source}
	rest := source

	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			if rest != "" {
				t.parts = append(t.parts, part{kind: kindLiteral, literal: rest})
			}
			break
		}
		if open > 0 {
			t.parts = append(t.parts, part{kind: kindLiteral, literal: rest[:open]})
		}
		after := rest[open+2:]
		closeAt := strings.Index(after, "}}")
		if closeAt < 0 {
			return nil, errs.New(errs.CodeConfigTemplateSyntax,
				"unclosed template token in %q", truncate(source)).
				WithHint("every {{ needs a matching }}")
		}
		token := strings.TrimSpace(after[:closeAt])
		p, err := compileToken(token, source)
		if err != nil {
			return nil, err
		}
		t.parts = append(t.parts, p)
		rest = after[closeAt+2:]
	}

	// Exactly one token and nothing around it: the value keeps its own type.
	if len(t.parts) == 1 && t.parts[0].kind != kindLiteral {
		t.solo = &t.parts[0]
	}
	return t, nil
}

func compileToken(token, source string) (part, error) {
	if token == "" {
		return part{}, errs.New(errs.CodeConfigTemplateSyntax, "empty template token in %q", truncate(source)).
			WithHint("write {{name}} for a variable, or a generator such as {{uuid}}")
	}

	name, rawArgs, hasArgs := strings.Cut(token, " ")
	rawArgs = strings.TrimSpace(rawArgs)

	if spec, ok := knownFuncs[name]; ok {
		return compileGenerator(name, rawArgs, spec.minArgs, spec.maxArgs, source)
	}

	// A token with arguments can only be a function call, so an unrecognised name is a
	// mistake rather than a variable that happens to contain spaces.
	if hasArgs {
		return part{}, unknownFunction(name, source)
	}
	// A bare token that differs from a generator only in case is almost certainly a
	// misspelled generator, not a variable someone meant to extract.
	for known := range knownFuncs {
		if strings.EqualFold(known, name) {
			return part{}, unknownFunction(name, source)
		}
	}

	if feeder, column, ok := strings.Cut(name, "."); ok {
		if feeder == "" || column == "" {
			return part{}, errs.New(errs.CodeConfigTemplateSyntax,
				"%q is not a valid feeder reference", token).
				WithHint("write {{feedername.column}}")
		}
		return part{kind: kindFeeder, name: feeder, column: column}, nil
	}
	if !isIdentifier(name) {
		return part{}, errs.New(errs.CodeConfigTemplateSyntax,
			"%q is not a valid variable name", name).
			WithHint("variable names are letters, digits and underscores, and cannot start with a digit")
	}
	return part{kind: kindVar, name: name}, nil
}

func compileGenerator(name, rawArgs string, minArgs, maxArgs int, source string) (part, error) {
	p := part{kind: kindGen, name: name}

	// pick takes its alternatives as one argument separated by pipes, so it is split
	// differently from the others - a choice may legitimately contain a space.
	if name == fnPick {
		if rawArgs == "" {
			return part{}, badArgs(name, "needs alternatives", source)
		}
		for _, choice := range strings.Split(rawArgs, "|") {
			choice = strings.TrimSpace(choice)
			if choice == "" {
				return part{}, badArgs(name, "has an empty alternative", source)
			}
			p.picks = append(p.picks, choice)
		}
		if len(p.picks) < 2 {
			return part{}, badArgs(name, "needs at least two alternatives separated by |", source)
		}
		return p, nil
	}

	if rawArgs != "" {
		p.args = strings.Fields(rawArgs)
	}
	if len(p.args) < minArgs || len(p.args) > maxArgs {
		return part{}, badArgs(name, fmt.Sprintf("takes %s, got %d", argCount(minArgs, maxArgs), len(p.args)), source)
	}

	switch name {
	case fnRandInt:
		lo, err := strconv.ParseInt(p.args[0], 10, 64)
		if err != nil {
			return part{}, badArgs(name, fmt.Sprintf("has a non-numeric minimum %q", p.args[0]), source)
		}
		hi, err := strconv.ParseInt(p.args[1], 10, 64)
		if err != nil {
			return part{}, badArgs(name, fmt.Sprintf("has a non-numeric maximum %q", p.args[1]), source)
		}
		if lo > hi {
			return part{}, badArgs(name, fmt.Sprintf("has a minimum (%d) above its maximum (%d)", lo, hi), source)
		}
		p.lo, p.hi = lo, hi
	case fnRandString:
		n, err := strconv.Atoi(p.args[0])
		if err != nil || n <= 0 {
			return part{}, badArgs(name, fmt.Sprintf("needs a positive length, got %q", p.args[0]), source)
		}
		if n > maxRandStringLength {
			return part{}, badArgs(name, fmt.Sprintf("length %d is above the %d limit", n, maxRandStringLength), source)
		}
		p.length = n
	}
	return p, nil
}

// maxRandStringLength bounds a generated string. Without a limit a typo turns into a
// multi-megabyte request body sent thousands of times a second.
const maxRandStringLength = 1 << 20

func argCount(minArgs, maxArgs int) string {
	if minArgs == maxArgs {
		return fmt.Sprintf("%d argument(s)", minArgs)
	}
	return fmt.Sprintf("%d to %d arguments", minArgs, maxArgs)
}

func unknownFunction(name, source string) *errs.Error {
	e := errs.New(errs.CodeConfigTemplateUnknownFn, "unknown generator %q", name).
		WithHint("available generators: %s", strings.Join(FunctionNames(), ", "))
	for known, spec := range knownFuncs {
		if strings.EqualFold(known, name) {
			return e.WithHint("did you mean %q? %s", known, spec.usage)
		}
	}
	_ = source
	return e
}

func badArgs(name, problem, source string) *errs.Error {
	return errs.New(errs.CodeConfigTemplateSyntax, "%s %s", name, problem).
		WithHint("%s", knownFuncs[name].usage).
		WithDetail("template", truncate(source))
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func truncate(s string) string {
	const limit = 120
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// Source is the template as written.
func (t *Template) Source() string { return t.source }

// IsStatic reports whether the template has no tokens at all, so it can be used
// without a rendering context. A wrapped literal value is not static: it renders to
// itself but carries a type, and Static returns only text.
func (t *Template) IsStatic() bool {
	for _, p := range t.parts {
		if p.kind != kindLiteral {
			return false
		}
	}
	return true
}

// Static returns the template's text, valid only when IsStatic.
func (t *Template) Static() string { return t.source }

// Vars lists the variables this template reads, for the static dataflow check.
func (t *Template) Vars() []string { return t.namesOf(kindVar) }

// Feeders lists the feeders this template reads.
func (t *Template) Feeders() []string { return t.namesOf(kindFeeder) }

// FeederColumns lists the feeder columns this template reads, as "feeder.column".
func (t *Template) FeederColumns() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range t.parts {
		if p.kind != kindFeeder {
			continue
		}
		key := p.name + "." + p.column
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func (t *Template) namesOf(k kind) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range t.parts {
		if p.kind == k && !seen[p.name] {
			seen[p.name] = true
			out = append(out, p.name)
		}
	}
	sort.Strings(out)
	return out
}

// Literal wraps an already-typed value as a template that renders to itself.
//
// A bind argument written as a number in the configuration is already a value, not a
// string to be parsed. Wrapping it keeps one render path for every argument instead of
// making each caller branch on whether templating applies.
func Literal(v any) *Template {
	return &Template{
		source: fmt.Sprint(v),
		parts:  []part{{kind: kindValue, value: v}},
		solo:   &part{kind: kindValue, value: v},
	}
}
