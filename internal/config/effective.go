package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// EffectiveYAML renders the configuration a run executed, for config.effective.yaml
// (§6.4): the document as written with the --set overrides applied, ${ENV} references
// preserved rather than resolved, and every secret supplied inline redacted.
//
// Keeping references is what lets a run be reproduced with `run --from` without its
// secrets ever being written to disk. Defaults are not written: they are derived again,
// identically, when the file is loaded, and writing them would turn a `rate:` shorthand
// into a rate and a stage list that contradict each other.
//
// A reference that stands where a number or a duration belongs cannot be decoded while
// it is still a reference. Then the file holds resolved, redacted values instead, and
// preserved is false so the caller can say so.
func EffectiveYAML(ctx context.Context, raw []byte, overrides []string, lookup func(string) (string, bool)) (doc []byte, preserved bool, err error) {
	keep := func(string) (string, bool) { return "", false }
	cfg, kerr := loadRaw(ctx, raw, keep, true)
	if kerr == nil {
		kerr = cfg.ApplySet(overrides)
	}
	preserved = kerr == nil
	if !preserved {
		cfg, err = loadRaw(ctx, raw, lookup, false)
		if err != nil {
			return nil, false, err
		}
		if e := cfg.ApplySet(overrides); e != nil {
			return nil, false, e
		}
	}

	// Through JSON, whose tags omit what was never set, so the file reads like the
	// document that was written rather than like every field the schema has.
	js, err := json.Marshal(cfg.Redacted())
	if err != nil {
		return nil, false, errs.Wrap(errs.CodeInternal, err, "encoding the effective configuration")
	}
	var plain any
	if e := yaml.Unmarshal(js, &plain); e != nil {
		return nil, false, errs.Wrap(errs.CodeInternal, e, "encoding the effective configuration")
	}
	body, err := yaml.Marshal(pruneZero(plain))
	if err != nil {
		return nil, false, errs.Wrap(errs.CodeInternal, err, "encoding the effective configuration")
	}
	var head bytes.Buffer
	head.WriteString("# The configuration this run executed: as written, with its --set overrides applied.\n")
	if preserved {
		head.WriteString("# Environment references are kept as written; inline secrets are redacted and must be\n")
		head.WriteString("# supplied again with --set or the environment when re-running with --from.\n")
	} else {
		head.WriteString("# An environment reference stood where a number or duration belongs, so values here are\n")
		head.WriteString("# resolved; secrets are still redacted.\n")
	}
	if len(overrides) > 0 {
		head.WriteString("# Overrides: " + strings.Join(overrides, " ") + "\n")
	}
	return append(head.Bytes(), body...), preserved, nil
}

// loadRaw decodes a document without defaults or validation. With keepRefs, every
// ${ENV} reference is left exactly as written.
func loadRaw(ctx context.Context, raw []byte, lookup func(string) (string, bool), keepRefs bool) (*Config, error) {
	return Load(ctx, FromBytes("config.effective.yaml", raw), Options{Lookup: lookup, Raw: true, keepRefs: keepRefs})
}

// RedactedPaths lists the places a loaded configuration still holds the redaction
// marker - secrets a stored configuration could not keep, which a re-run must supply.
func (c *Config) RedactedPaths() []string {
	b, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil
	}
	var out []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				walk(path+"."+k, child)
			}
		case []any:
			for i, child := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		case string:
			// A URL escapes the brackets, so a redacted DSN password reads %5Bredacted%5D.
			if strings.Contains(t, Redacted) || strings.Contains(t, url.PathEscape(Redacted)) {
				out = append(out, strings.TrimPrefix(path, "."))
			}
		}
	}
	walk("", doc)
	return out
}

// pruneZero drops values the document never set: empty strings, zero numbers and
// durations, false, and empty collections. Each is what the loader defaults an absent
// key to, so leaving it out changes nothing on reload and keeps the file readable as
// the document that was written.
func pruneZero(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range t {
			if p := pruneZero(child); !isZero(p) {
				out[k] = p
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, child := range t {
			out = append(out, pruneZero(child))
		}
		return out
	default:
		return v
	}
}

func isZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == "" || t == "0s"
	case bool:
		return !t
	case int:
		return t == 0
	case float64:
		return t == 0
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	default:
		return false
	}
}
