package template_test

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/template"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var epoch = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func newContext() *template.Context {
	return &template.Context{
		Shared: template.NewShared(clock.NewFake(epoch)),
		Rand:   rand.New(rand.NewPCG(42, 7)),
		Vars:   map[string]string{},
		Rows:   map[string]map[string]string{},
	}
}

func compile(t *testing.T, src string) *template.Template {
	t.Helper()
	tpl, err := template.Compile(src)
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	return tpl
}

func render(t *testing.T, src string, ctx *template.Context) string {
	t.Helper()
	got, err := compile(t, src).Render(ctx)
	if err != nil {
		t.Fatalf("Render(%q): %v", src, err)
	}
	return got
}

func TestStaticTemplatePassesThrough(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "/api/items", "SELECT * FROM items", "a } b { c"} {
		tpl := compile(t, s)
		if !tpl.IsStatic() {
			t.Errorf("%q should be static", s)
		}
		if got := render(t, s, nil); got != s {
			t.Errorf("Render(%q) = %q", s, got)
		}
	}
}

func TestGenerators(t *testing.T) {
	t.Parallel()
	ctx := newContext()

	t.Run("randInt is within its inclusive range", func(t *testing.T) {
		tpl := compile(t, "{{randInt 5 7}}")
		seen := map[int64]bool{}
		for range 300 {
			v, err := tpl.RenderValue(ctx)
			if err != nil {
				t.Fatalf("RenderValue: %v", err)
			}
			n, ok := v.(int64)
			if !ok {
				t.Fatalf("randInt produced %T, want int64", v)
			}
			if n < 5 || n > 7 {
				t.Fatalf("randInt 5 7 produced %d", n)
			}
			seen[n] = true
		}
		// Inclusive at both ends, which is what a reader expects.
		for _, want := range []int64{5, 6, 7} {
			if !seen[want] {
				t.Errorf("randInt 5 7 never produced %d in 300 draws", want)
			}
		}
	})

	t.Run("randInt with an empty range", func(t *testing.T) {
		v, err := compile(t, "{{randInt 9 9}}").RenderValue(ctx)
		if err != nil {
			t.Fatalf("RenderValue: %v", err)
		}
		if v != int64(9) {
			t.Errorf("randInt 9 9 = %v", v)
		}
	})

	t.Run("randString has the requested length", func(t *testing.T) {
		got := render(t, "{{randString 12}}", ctx)
		if len(got) != 12 {
			t.Errorf("randString 12 produced %q (%d characters)", got, len(got))
		}
		for _, r := range got {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", r) {
				t.Errorf("randString produced %q, which contains %q", got, r)
			}
		}
	})

	t.Run("uuid is well formed and does not repeat", func(t *testing.T) {
		seen := map[string]bool{}
		for range 200 {
			got := render(t, "{{uuid}}", ctx)
			if len(got) != 36 || got[8] != '-' || got[13] != '-' || got[18] != '-' || got[23] != '-' {
				t.Fatalf("uuid produced %q", got)
			}
			if got[14] != '4' {
				t.Errorf("uuid %q is not version 4", got)
			}
			if v := got[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
				t.Errorf("uuid %q has the wrong variant nibble %q", got, v)
			}
			if seen[got] {
				t.Fatalf("uuid repeated: %q", got)
			}
			seen[got] = true
		}
	})

	t.Run("seq counts up across the run", func(t *testing.T) {
		fresh := newContext()
		tpl := compile(t, "{{seq}}")
		for want := int64(1); want <= 5; want++ {
			v, err := tpl.RenderValue(fresh)
			if err != nil {
				t.Fatalf("RenderValue: %v", err)
			}
			if v != want {
				t.Fatalf("seq = %v, want %v", v, want)
			}
		}
	})

	t.Run("pick chooses among its alternatives", func(t *testing.T) {
		seen := map[string]int{}
		for range 300 {
			seen[render(t, "{{pick red|green|blue}}", ctx)]++
		}
		for _, want := range []string{"red", "green", "blue"} {
			if seen[want] == 0 {
				t.Errorf("pick never chose %q", want)
			}
		}
		if len(seen) != 3 {
			t.Errorf("pick produced %v, want exactly the three alternatives", seen)
		}
	})

	t.Run("pick allows spaces in an alternative", func(t *testing.T) {
		seen := map[string]bool{}
		for range 200 {
			seen[render(t, "{{pick new york|san francisco}}", ctx)] = true
		}
		if !seen["new york"] || !seen["san francisco"] {
			t.Errorf("pick with spaces produced %v", seen)
		}
	})

	t.Run("now reads the injected clock", func(t *testing.T) {
		v, err := compile(t, "{{now}}").RenderValue(ctx)
		if err != nil {
			t.Fatalf("RenderValue: %v", err)
		}
		got, ok := v.(time.Time)
		if !ok {
			t.Fatalf("now produced %T, want time.Time", v)
		}
		if !got.Equal(epoch) {
			t.Errorf("now = %v, want the fake clock's %v", got, epoch)
		}
	})
}

// A template that is exactly one generator keeps its natural type, so a SQL argument
// binds as an integer rather than as the string "42" - which some drivers reject and
// others accept while silently defeating an index.
func TestSoloTokenKeepsItsType(t *testing.T) {
	t.Parallel()
	ctx := newContext()
	cases := []struct {
		src  string
		want string // the Go type name
	}{
		{"{{randInt 1 10}}", "int64"},
		{"{{seq}}", "int64"},
		{"{{uuid}}", "string"},
		{"{{randString 4}}", "string"},
		{"{{pick a|b}}", "string"},
		{"{{now}}", "time.Time"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			v, err := compile(t, tc.src).RenderValue(ctx)
			if err != nil {
				t.Fatalf("RenderValue: %v", err)
			}
			if got := typeName(v); got != tc.want {
				t.Errorf("%s produced %s, want %s", tc.src, got, tc.want)
			}
		})
	}

	// Anything with surrounding text is a string, because that is what it is.
	v, err := compile(t, "item-{{randInt 1 10}}").RenderValue(ctx)
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("a token with surrounding text produced %T, want string", v)
	}
	if !strings.HasPrefix(s, "item-") {
		t.Errorf("got %q", s)
	}
}

func typeName(v any) string {
	switch v.(type) {
	case int64:
		return "int64"
	case string:
		return "string"
	case time.Time:
		return "time.Time"
	default:
		return "unknown"
	}
}

func TestMixedTemplate(t *testing.T) {
	t.Parallel()
	ctx := newContext()
	ctx.Vars["order_id"] = "abc-123"
	got := render(t, "/api/orders/{{order_id}}/items/{{randInt 3 3}}", ctx)
	if got != "/api/orders/abc-123/items/3" {
		t.Errorf("got %q", got)
	}
}

func TestVariablesAndFeeders(t *testing.T) {
	t.Parallel()
	t.Run("a variable resolves from the context", func(t *testing.T) {
		ctx := newContext()
		ctx.Vars["token"] = "xyz"
		if got := render(t, "Bearer {{token}}", ctx); got != "Bearer xyz" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a feeder column resolves from the row", func(t *testing.T) {
		ctx := newContext()
		ctx.Rows["users"] = map[string]string{"email": "a@example.com"}
		if got := render(t, "{{users.email}}", ctx); got != "a@example.com" {
			t.Errorf("got %q", got)
		}
	})

	// A literal {{token}} must never reach a target: a request for a resource that
	// does not exist would show up as a 404 and look like a target problem (§4).
	t.Run("an unresolved variable fails rather than rendering literally", func(t *testing.T) {
		_, err := compile(t, "/api/{{missing}}").Render(newContext())
		if err == nil {
			t.Fatal("an unresolved variable rendered instead of failing")
		}
		var typed *errs.Error
		if !errors.As(err, &typed) || typed.Code != errs.CodeConfigUndefinedVariable {
			t.Errorf("code = %v, want %s", err, errs.CodeConfigUndefinedVariable)
		}
	})

	t.Run("an unknown feeder fails", func(t *testing.T) {
		_, err := compile(t, "{{nosuch.column}}").Render(newContext())
		if err == nil {
			t.Fatal("an unknown feeder rendered instead of failing")
		}
	})

	t.Run("a missing column fails", func(t *testing.T) {
		ctx := newContext()
		ctx.Rows["users"] = map[string]string{"email": "a@example.com"}
		if _, err := compile(t, "{{users.name}}").Render(ctx); err == nil {
			t.Fatal("a missing column rendered instead of failing")
		}
	})
}

func TestCompileErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		code errs.Code
	}{
		{"unclosed token", "/api/{{randInt 1 2", errs.CodeConfigTemplateSyntax},
		{"empty token", "{{}}", errs.CodeConfigTemplateSyntax},
		{"unknown generator with args", "{{frobnicate 1 2}}", errs.CodeConfigTemplateUnknownFn},
		{"misspelled generator", "{{randint 1 2}}", errs.CodeConfigTemplateUnknownFn},
		{"wrong case, no args", "{{UUID}}", errs.CodeConfigTemplateUnknownFn},
		{"randInt with one argument", "{{randInt 5}}", errs.CodeConfigTemplateSyntax},
		{"randInt with three arguments", "{{randInt 1 2 3}}", errs.CodeConfigTemplateSyntax},
		{"randInt with a non-number", "{{randInt a 5}}", errs.CodeConfigTemplateSyntax},
		{"randInt reversed", "{{randInt 10 1}}", errs.CodeConfigTemplateSyntax},
		{"randString with no length", "{{randString}}", errs.CodeConfigTemplateSyntax},
		{"randString with zero length", "{{randString 0}}", errs.CodeConfigTemplateSyntax},
		{"randString absurdly long", "{{randString 99999999}}", errs.CodeConfigTemplateSyntax},
		{"pick with no alternatives", "{{pick}}", errs.CodeConfigTemplateSyntax},
		{"pick with one alternative", "{{pick only}}", errs.CodeConfigTemplateSyntax},
		{"pick with an empty alternative", "{{pick a||b}}", errs.CodeConfigTemplateSyntax},
		// A known generator given the wrong arity is a syntax error, not an unknown one.
		{"uuid with an argument", "{{uuid 4}}", errs.CodeConfigTemplateSyntax},
		{"bad variable name", "{{no-hyphens}}", errs.CodeConfigTemplateSyntax},
		{"bad feeder reference", "{{users.}}", errs.CodeConfigTemplateSyntax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := template.Compile(tc.src)
			if err == nil {
				t.Fatalf("Compile(%q) succeeded, want an error", tc.src)
			}
			var typed *errs.Error
			if !errors.As(err, &typed) {
				t.Fatalf("not a coded error: %v", err)
			}
			if typed.Code != tc.code {
				t.Errorf("code = %s, want %s (%s)", typed.Code, tc.code, typed.Message)
			}
			if typed.Hint == "" {
				t.Error("a template error should say what to write instead")
			}
		})
	}
}

// A misspelled generator should point at the one that was meant, since the whole
// reason to compile at load time is to catch this before a run is wasted.
func TestMisspelledGeneratorSuggestsTheRightOne(t *testing.T) {
	t.Parallel()
	_, err := template.Compile("{{randint 1 5}}")
	var typed *errs.Error
	if !errors.As(err, &typed) {
		t.Fatalf("not a coded error: %v", err)
	}
	if !strings.Contains(typed.Hint, "randInt") {
		t.Errorf("hint = %q, want it to suggest randInt", typed.Hint)
	}
}

func TestIntrospection(t *testing.T) {
	t.Parallel()
	tpl := compile(t, "{{a}}/{{b}}/{{users.email}}/{{randInt 1 2}}/{{a}}")

	if got := tpl.Vars(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Vars() = %v, want [a b] with no duplicates", got)
	}
	if got := tpl.Feeders(); len(got) != 1 || got[0] != "users" {
		t.Errorf("Feeders() = %v, want [users]", got)
	}
	if got := tpl.FeederColumns(); len(got) != 1 || got[0] != "users.email" {
		t.Errorf("FeederColumns() = %v", got)
	}
	if tpl.IsStatic() {
		t.Error("a template with tokens is not static")
	}
	if got := tpl.Source(); !strings.Contains(got, "{{a}}") {
		t.Errorf("Source() lost the original: %q", got)
	}
}

// The same seed must produce the same values, or a run cannot be reproduced from the
// seed recorded in its own result.
func TestGeneratorsAreReproducible(t *testing.T) {
	t.Parallel()
	collect := func() []string {
		ctx := &template.Context{
			Shared: template.NewShared(clock.NewFake(epoch)),
			Rand:   rand.New(rand.NewPCG(99, 1)),
		}
		tpl := compile(t, "{{randInt 1 1000000}}-{{uuid}}-{{randString 8}}-{{pick a|b|c}}")
		out := make([]string, 0, 20)
		for range 20 {
			s, err := tpl.Render(ctx)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			out = append(out, s)
		}
		return out
	}
	first, second := collect(), collect()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("the same seed diverged at %d: %q vs %q", i, first[i], second[i])
		}
	}
}

// Compiling happens once at load; rendering happens on every operation, so it must not
// be doing parsing work.
func BenchmarkRender(b *testing.B) {
	tpl, err := template.Compile("/api/items/{{randInt 1 100000}}?token={{randString 8}}")
	if err != nil {
		b.Fatalf("Compile: %v", err)
	}
	ctx := &template.Context{
		Shared: template.NewShared(clock.New()),
		Rand:   rand.New(rand.NewPCG(1, 2)),
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := tpl.Render(ctx); err != nil {
			b.Fatalf("Render: %v", err)
		}
	}
}

func BenchmarkRenderStatic(b *testing.B) {
	tpl, err := template.Compile("/api/items")
	if err != nil {
		b.Fatalf("Compile: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := tpl.Render(nil); err != nil {
			b.Fatalf("Render: %v", err)
		}
	}
}

// Fuzzing the compiler: it must never panic, and anything it accepts must render
// without panicking either. §10 lists the template compiler as a fuzz target.
func FuzzCompile(f *testing.F) {
	for _, seed := range []string{
		"", "plain", "{{uuid}}", "{{randInt 1 2}}", "{{", "}}", "{{}}", "{{{{}}}}",
		"{{pick a|b}}", "a{{seq}}b{{now}}c", "{{users.email}}", "{{randString 3}}",
		"{{randInt -5 -1}}", "{{ uuid }}", "{{a.b.c}}",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		tpl, err := template.Compile(src)
		if err != nil {
			return // rejecting input is a fine outcome; panicking is not
		}
		ctx := &template.Context{
			Shared: template.NewShared(clock.NewFake(epoch)),
			Rand:   rand.New(rand.NewPCG(1, 2)),
			Vars:   map[string]string{},
			Rows:   map[string]map[string]string{},
		}
		out, err := tpl.Render(ctx)
		if err != nil {
			return // an unresolved variable is an expected failure
		}
		// Whatever rendered must not still contain an unexpanded token.
		if strings.Contains(out, "{{") && !strings.Contains(src, "{{{{") {
			t.Errorf("Compile(%q).Render() left a token behind: %q", src, out)
		}
	})
}
