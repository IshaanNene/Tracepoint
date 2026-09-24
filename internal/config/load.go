package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Source is where a configuration comes from.
type Source struct {
	// Path is the file to read, or "-" for standard input.
	Path string
	// Data, when set, is used instead of reading anything. Path is then only a label
	// for error messages.
	Data []byte
}

// FromFile reads a configuration from a path, or from standard input for "-".
func FromFile(path string) Source { return Source{Path: path} }

// FromBytes uses an in-memory document. label appears in error messages.
func FromBytes(label string, data []byte) Source { return Source{Path: label, Data: data} }

// Options control loading.
type Options struct {
	// Lookup resolves ${ENV} references. Defaults to os.LookupEnv; tests and the
	// library API inject their own so loading never depends on process state.
	Lookup func(string) (string, bool)
	// Stdin is read for the "-" path.
	Stdin io.Reader
	// Raw returns the document as written: no defaults, no validation.
	//
	// It exists for the --set path. Overrides are input, at the top of the precedence
	// chain, so they have to be applied before defaults are derived from the values
	// they change - otherwise raising run.duration leaves the rate shorthand's stage
	// at the old length and the configuration contradicts itself. The caller finishes
	// with Finalise.
	Raw bool

	// keepRefs leaves ${ENV} references as written, for EffectiveYAML.
	keepRefs bool
}

// envRef matches ${NAME} and ${NAME:-default}.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// Load reads, interpolates, decodes, defaults and validates a configuration.
//
// The order matters. Interpolation happens on the raw text, before parsing, so a
// value may be any part of the document. Unknown fields are checked against the
// parsed document, so their positions are still known. Defaults are applied only
// after the document has been accepted, so an error always describes what was
// written rather than what it would have become.
func Load(ctx context.Context, src Source, opts Options) (*Config, error) {
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}

	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.CodeIOReadFailed, err, "loading the configuration")
	}

	raw, err := read(src, opts.Stdin)
	if err != nil {
		return nil, err
	}
	interpolated := raw
	if !opts.keepRefs {
		interpolated, err = Interpolate(raw, opts.Lookup)
		if err != nil {
			return nil, err
		}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(interpolated, &doc); err != nil {
		return nil, parseError(src.Path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, errs.New(errs.CodeConfigParse, "the configuration is empty").
			WithHint("start from `tracepoint init`, or see docs/CONFIG.md")
	}

	// Report every unknown field at once, each with its own position.
	if unknown := checkKnownFields(&doc, reflect.TypeOf(Config{})); len(unknown) > 0 {
		return nil, summarise(unknown)
	}

	var cfg Config
	if err := doc.Decode(&cfg); err != nil {
		return nil, decodeError(err)
	}
	if cfg.Version != Version {
		return nil, errs.New(errs.CodeConfigVersionUnsupported,
			"unsupported configuration version %d", cfg.Version).
			WithPath("/version").
			WithHint("this build understands version %d; add `version: %d` at the top of the file", Version, Version)
	}
	if opts.Raw {
		return &cfg, nil
	}
	if err := cfg.Finalise(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Finalise derives every default and then validates.
//
// Defaults come second so that an error always describes what was written, and
// validation comes last so it judges the configuration that will actually run.
func (c *Config) Finalise() error {
	c.ApplyDefaults()
	return c.Validate()
}

func read(src Source, stdin io.Reader) ([]byte, error) {
	if src.Data != nil {
		return src.Data, nil
	}
	if src.Path == "-" {
		if stdin == nil {
			stdin = os.Stdin
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, errs.Wrap(errs.CodeIOReadFailed, err, "reading the configuration from standard input")
		}
		return b, nil
	}
	b, err := os.ReadFile(src.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errs.Wrap(errs.CodeConfigNotFound, err, "no configuration at %s", src.Path).
				WithHint("check the path, or pass -c - to read the configuration from standard input")
		}
		return nil, errs.Wrap(errs.CodeIOReadFailed, err, "reading %s", src.Path)
	}
	return b, nil
}

// Interpolate expands ${NAME} and ${NAME:-default} in the raw document.
//
// It runs before parsing so a reference can stand anywhere - a whole section, a single
// character of a URL. A reference with no value and no default is an error rather than
// an empty string, because silently substituting nothing produces a configuration that
// parses, runs, and measures the wrong thing.
func Interpolate(raw []byte, lookup func(string) (string, bool)) ([]byte, error) {
	var missing []*errs.Error
	out := envRef.ReplaceAllFunc(raw, func(match []byte) []byte {
		groups := envRef.FindSubmatch(match)
		name := string(groups[1])
		if v, ok := lookup(name); ok {
			return []byte(v)
		}
		if len(groups[2]) > 0 { // a :- default was supplied
			return groups[3]
		}
		missing = append(missing, errs.New(errs.CodeConfigEnvUnset,
			"the environment variable %s is referenced but not set", name).
			WithPos(lineOf(raw, match), 0).
			WithHint("export %s, or write ${%s:-default} to supply a fallback", name, name))
		return match
	})
	if len(missing) > 0 {
		return nil, summarise(missing)
	}
	return out, nil
}

func lineOf(raw, match []byte) int {
	idx := bytes.Index(raw, match)
	if idx < 0 {
		return 0
	}
	return bytes.Count(raw[:idx], []byte("\n")) + 1
}

// summarise turns a set of problems into one error that carries them all, so a caller
// fixing a configuration sees every mistake at once.
func summarise(list []*errs.Error) error {
	if len(list) == 1 {
		return list[0]
	}
	head := errs.New(list[0].Code, "%d problems in the configuration", len(list))
	head.Hint = list[0].Hint
	return head.WithCauses(list...)
}

// yamlPos pulls the line number out of a go-yaml message, which embeds it as text.
var yamlPos = regexp.MustCompile(`line (\d+):`)

func parseError(path string, err error) error {
	e := errs.Wrap(errs.CodeConfigParse, err, "%s is not valid YAML or JSON", displayPath(path))
	if m := yamlPos.FindStringSubmatch(err.Error()); m != nil {
		e = e.WithPos(atoi(m[1]), 0)
	}
	return e.WithHint("check indentation and quoting; `tracepoint validate -c %s` reports each problem", displayPath(path))
}

// decodeError turns a type error from the decoder into a coded one. Unknown fields
// have already been reported with positions, so anything reaching here is a value of
// the wrong shape.
func decodeError(err error) error {
	msg := err.Error()
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) && len(typeErr.Errors) > 0 {
		list := make([]*errs.Error, 0, len(typeErr.Errors))
		for _, m := range typeErr.Errors {
			e := errs.New(errs.CodeConfigInvalidValue, "%s", cleanYAMLMessage(m))
			if pos := yamlPos.FindStringSubmatch(m); pos != nil {
				e = e.WithPos(atoi(pos[1]), 0)
			}
			list = append(list, e)
		}
		return summarise(list)
	}
	e := errs.Wrap(errs.CodeConfigInvalidValue, err, "%s", cleanYAMLMessage(msg))
	if pos := yamlPos.FindStringSubmatch(msg); pos != nil {
		e = e.WithPos(atoi(pos[1]), 0)
	}
	return e
}

func cleanYAMLMessage(m string) string {
	m = strings.TrimPrefix(m, "yaml: ")
	m = strings.TrimPrefix(m, "unmarshal errors:\n  ")
	if _, rest, ok := strings.Cut(m, ": "); ok && strings.HasPrefix(m, "line ") {
		return rest
	}
	return m
}

func displayPath(p string) string {
	if p == "-" {
		return "the configuration on standard input"
	}
	if p == "" {
		return "the configuration"
	}
	return p
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
