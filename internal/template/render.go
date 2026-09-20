package template

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// alphabet is what randString draws from: unambiguous in a log, safe in a URL and in
// a SQL literal without quoting surprises.
const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Shared is the per-run state every worker's rendering context points at.
type Shared struct {
	seq   atomic.Int64
	clock clock.Clock
}

// NewShared builds the shared state. The clock is injected so `now` is testable and so
// nothing in the render path reads the wall clock directly.
func NewShared(clk clock.Clock) *Shared {
	if clk == nil {
		clk = clock.New()
	}
	return &Shared{clock: clk}
}

// Context is what one iteration renders against.
//
// Rand is the worker's own source, so generators never contend on a shared one and the
// whole run stays reproducible from its recorded seed. Vars holds values extracted by
// earlier steps of the same journey; Rows holds the feeder rows picked for this
// iteration.
type Context struct {
	Shared *Shared
	Rand   *rand.Rand
	Vars   map[string]string
	Rows   map[string]map[string]string
}

// Render produces the template's text.
func (t *Template) Render(ctx *Context) (string, error) {
	if t.IsStatic() {
		return t.source, nil
	}
	var b strings.Builder
	b.Grow(len(t.source) + 16)
	for i := range t.parts {
		p := &t.parts[i]
		if p.kind == kindLiteral {
			b.WriteString(p.literal)
			continue
		}
		s, err := p.renderString(ctx)
		if err != nil {
			return "", err
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// RenderValue produces a value that keeps its natural type when the template is
// exactly one token.
//
// This is what lets `args: ["{{randInt 1 100000}}"]` bind to SQL as an integer. Binding
// it as the string "42" would work on some drivers, fail on others, and quietly defeat
// an index on the column - which would look like a slow database rather than a badly
// bound parameter.
func (t *Template) RenderValue(ctx *Context) (any, error) {
	if t.solo == nil {
		return t.Render(ctx)
	}
	return t.solo.renderValue(ctx)
}

func (p *part) renderString(ctx *Context) (string, error) {
	v, err := p.renderValue(ctx)
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano), nil
	default:
		return "", errs.New(errs.CodeInternal, "a generator produced an unexpected %T", v)
	}
}

func (p *part) renderValue(ctx *Context) (any, error) {
	switch p.kind {
	case kindLiteral:
		return p.literal, nil

	case kindVar:
		if ctx == nil || ctx.Vars == nil {
			return nil, undefinedVar(p.name)
		}
		v, ok := ctx.Vars[p.name]
		if !ok {
			return nil, undefinedVar(p.name)
		}
		return v, nil

	case kindFeeder:
		if ctx == nil || ctx.Rows == nil {
			return nil, missingFeeder(p.name, p.column)
		}
		row, ok := ctx.Rows[p.name]
		if !ok {
			return nil, missingFeeder(p.name, p.column)
		}
		v, ok := row[p.column]
		if !ok {
			return nil, errs.New(errs.CodeConfigFeederNotFound,
				"feeder %q has no column %q", p.name, p.column)
		}
		return v, nil

	case kindGen:
		return p.generate(ctx)

	default:
		return nil, errs.New(errs.CodeInternal, "unhandled template part")
	}
}

func (p *part) generate(ctx *Context) (any, error) {
	if ctx == nil || ctx.Rand == nil || ctx.Shared == nil {
		return nil, errs.New(errs.CodeInternal, "a generator needs a rendering context")
	}
	switch p.name {
	case fnRandInt:
		// Inclusive at both ends, which is what a reader of "randInt 1 100" expects.
		span := p.hi - p.lo
		if span == 0 {
			return p.lo, nil
		}
		return p.lo + ctx.Rand.Int64N(span+1), nil

	case fnRandString:
		b := make([]byte, p.length)
		for i := range b {
			b[i] = alphabet[ctx.Rand.IntN(len(alphabet))]
		}
		return string(b), nil

	case fnUUID:
		return uuidV4(ctx.Rand), nil

	case fnSeq:
		return ctx.Shared.seq.Add(1), nil

	case fnPick:
		return p.picks[ctx.Rand.IntN(len(p.picks))], nil

	case fnNow:
		return ctx.Shared.clock.Now(), nil

	default:
		return nil, errs.New(errs.CodeInternal, "unhandled generator %q", p.name)
	}
}

// uuidV4 formats a random version 4 UUID from the run's own seeded source, so a run is
// reproducible. It is an identifier, not a secret: nothing here depends on it being
// unpredictable.
func uuidV4(rng *rand.Rand) string {
	var b [16]byte
	hi, lo := rng.Uint64(), rng.Uint64()
	for i := range 8 {
		// Masking makes the truncation explicit: each iteration takes one byte out of
		// the 64-bit draw, which is the whole point.
		b[i] = byte((hi >> (8 * i)) & 0xff)
		b[8+i] = byte((lo >> (8 * i)) & 0xff)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	const hexDigits = "0123456789abcdef"
	out := make([]byte, 36)
	pos := 0
	for i, x := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[pos] = '-'
			pos++
		}
		out[pos] = hexDigits[x>>4]
		out[pos+1] = hexDigits[x&0x0f]
		pos += 2
	}
	return string(out)
}

func undefinedVar(name string) error {
	return errs.New(errs.CodeConfigUndefinedVariable,
		"no value for {{%s}}", name).
		WithHint("a variable must be extracted by an earlier step in the same journey, or supplied by a feeder")
}

func missingFeeder(feeder, column string) error {
	return errs.New(errs.CodeConfigFeederNotFound,
		"no feeder named %q to supply {{%s.%s}}", feeder, feeder, column).
		WithHint("declare it under http.feeders")
}
