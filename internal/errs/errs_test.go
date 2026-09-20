package errs_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// The code registry and docs/ERRORS.md are two halves of one contract: the registry
// is what the binary reports and the document is what a user or an agent reads to
// understand it. Drift between them is a documentation bug that looks like a product
// bug, so it fails the build.
func TestEveryCodeIsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/ERRORS.md")
	if err != nil {
		t.Fatalf("reading the error registry document: %v", err)
	}
	text := string(doc)
	for _, code := range errs.Codes() {
		if !strings.Contains(text, "`"+string(code)+"`") {
			t.Errorf("code %s is in the registry but not documented in docs/ERRORS.md", code)
		}
	}
}

func TestExitCodesFollowTheContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code Code
		want int
	}{
		{errs.CodeConfigUnknownField, errs.ExitUsage},
		{errs.CodePolicyTargetNotAllowed, errs.ExitUsage},
		{errs.CodePreflightConnect, errs.ExitRuntime},
		{errs.CodeRunAborted, errs.ExitRuntime},
		{errs.CodeRunInvalid, errs.ExitInvalid},
		{errs.CodeSLOBreach, errs.ExitBreach},
		{errs.CodeWaitTimeout, errs.ExitWaitOpen},
	}
	for _, c := range cases {
		t.Run(string(c.code), func(t *testing.T) {
			t.Parallel()
			if got := errs.New(c.code, "x").ExitCode(); got != c.want {
				t.Errorf("exit code = %d, want %d", got, c.want)
			}
		})
	}
}

// Spec §6.1 fixes precedence as 2 > 3 > 4 > 1 when several apply at once.
func TestWorstExitCodeFollowsPrecedence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []int
		want int
	}{
		{"usage beats everything", []int{errs.ExitBreach, errs.ExitInvalid, errs.ExitRuntime, errs.ExitUsage}, errs.ExitUsage},
		{"runtime beats invalid and breach", []int{errs.ExitBreach, errs.ExitInvalid, errs.ExitRuntime}, errs.ExitRuntime},
		{"invalid beats breach", []int{errs.ExitBreach, errs.ExitInvalid}, errs.ExitInvalid},
		{"breach alone", []int{errs.ExitBreach}, errs.ExitBreach},
		{"nothing wrong", []int{errs.ExitOK, errs.ExitOK}, errs.ExitOK},
		{"empty", nil, errs.ExitOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := errs.Worst(c.in...); got != c.want {
				t.Errorf("Worst(%v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestErrorWrappingSupportsIsAndAs(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("connection refused")
	err := errs.Wrap(errs.CodePreflightConnect, sentinel, "connecting to %s", "127.0.0.1:8080")

	if !errors.Is(err, sentinel) {
		t.Error("errors.Is did not find the wrapped cause")
	}
	var typed *errs.Error
	if !errors.As(error(err), &typed) || typed.Code != errs.CodePreflightConnect {
		t.Error("errors.As did not recover the typed error")
	}
	if got := err.Error(); !strings.Contains(got, "connection refused") {
		t.Errorf("message %q lost the underlying cause", got)
	}
}

func TestEnvelopeShape(t *testing.T) {
	t.Parallel()
	err := errs.New(errs.CodeConfigUnknownField, `unknown field %q`, "metod").
		WithPath("/http/requests/0/metod").
		WithPos(12, 7).
		WithHint(`did you mean %q?`, "method")

	b, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("marshalling: %v", marshalErr)
	}

	var got map[string]map[string]any
	if unmarshalErr := json.Unmarshal(b, &got); unmarshalErr != nil {
		t.Fatalf("unmarshalling: %v", unmarshalErr)
	}
	e, ok := got["error"]
	if !ok {
		t.Fatalf("envelope has no top-level \"error\" key: %s", b)
	}
	for key, want := range map[string]any{
		"code":      "CONFIG_UNKNOWN_FIELD",
		"message":   `unknown field "metod"`,
		"path":      "/http/requests/0/metod",
		"line":      float64(12),
		"column":    float64(7),
		"hint":      `did you mean "method"?`,
		"exit_code": float64(2),
		"retriable": false,
	} {
		if e[key] != want {
			t.Errorf("error.%s = %#v, want %#v", key, e[key], want)
		}
	}
}

// A stray error from outside this package must still leave stdout holding exactly one
// valid JSON document, because an agent parsing that stream has no fallback.
func TestEnvelopeOfForeignError(t *testing.T) {
	t.Parallel()
	env := errs.EnvelopeOf(errors.New("something from a library"))
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if !strings.Contains(string(b), `"code":"INTERNAL"`) {
		t.Errorf("foreign error did not map to INTERNAL: %s", b)
	}
	if errs.ExitCodeOf(errors.New("x")) != errs.ExitRuntime {
		t.Error("a foreign error should exit 3, not 0")
	}
}

func TestCausesCarrySiblingProblems(t *testing.T) {
	t.Parallel()
	err := errs.New(errs.CodeConfigInvalidValue, "2 problems in the configuration").
		WithCauses(
			errs.New(errs.CodeConfigInvalidValue, "weight must be above 0").WithPath("/http/requests/0/weight"),
			errs.New(errs.CodeConfigUnknownField, `unknown field "metod"`).WithPath("/http/requests/1/metod"),
		)
	b, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("marshalling: %v", marshalErr)
	}
	var env struct {
		Error struct {
			Causes []struct {
				Code string `json:"code"`
				Path string `json:"path"`
			} `json:"causes"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(b, &env); unmarshalErr != nil {
		t.Fatalf("unmarshalling: %v", unmarshalErr)
	}
	if len(env.Error.Causes) != 2 {
		t.Fatalf("got %d causes, want 2", len(env.Error.Causes))
	}
	if env.Error.Causes[1].Path != "/http/requests/1/metod" {
		t.Errorf("second cause lost its path: %+v", env.Error.Causes[1])
	}
}

// Code is aliased locally so the table above reads cleanly.
type Code = errs.Code
