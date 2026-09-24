package session_test

import (
	"errors"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/session"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// Precedence 3 > 4 > 1 (§6.1): cut short, then unbelievable, then over budget.
func TestExitForPrecedence(t *testing.T) {
	mk := func(status, validity string, pass bool) *result.Result {
		return &result.Result{Run: result.Run{Status: status},
			Analysis: result.Analysis{Validity: result.Validity{State: validity}, SLO: result.SLO{Pass: pass}}}
	}
	cases := []struct {
		name  string
		r     *result.Result
		allow bool
		code  int
		err   errs.Code
	}{
		{"clean", mk("completed", "valid", true), false, 0, ""},
		{"breach", mk("completed", "valid", false), false, 1, errs.CodeSLOBreach},
		{"invalid beats breach", mk("completed", "invalid", false), false, 4, errs.CodeRunInvalid},
		{"allow-invalid reveals the breach", mk("completed", "invalid", false), true, 1, errs.CodeSLOBreach},
		{"aborted beats invalid", mk("aborted", "invalid", false), false, 3, errs.CodeRunAborted},
		{"interrupted", mk("interrupted", "valid", true), false, 3, errs.CodeRunInterrupted},
	}
	for _, c := range cases {
		code, err := session.ExitFor(c.r, c.allow)
		var typed *errs.Error
		gotCode := errs.Code("")
		if errors.As(err, &typed) {
			gotCode = typed.Code
		}
		if code != c.code || gotCode != c.err {
			t.Errorf("%s: got %d %s, want %d %s", c.name, code, gotCode, c.code, c.err)
		}
	}
}

func TestPeakRates(t *testing.T) {
	cfg := &config.Config{
		HTTP: &config.HTTP{Executor: config.Executor{Stages: []config.Stage{{Target: 50}, {Target: 200}, {Target: 20}}}},
		DB:   &config.DB{Executor: config.Executor{Rate: 40}},
	}
	got := session.PeakRates(cfg)
	if got["http"] != 200 || got["db"] != 40 {
		t.Fatalf("peak rates = %v", got)
	}
}
