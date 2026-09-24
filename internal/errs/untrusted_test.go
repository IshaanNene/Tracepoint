package errs_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

func TestCleanUntrusted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain message", "plain message"},
		{"\x1b[31mred\x1b[0m text", "red text"},
		{"line one\nline two\r\n", "line one line two"},
		{"bell\x07 and nul\x00", "bell and nul"},
		{"zero\u200bwidth", "zerowidth"},
		{"bad \xff byte", "bad  byte"},
	}
	for _, c := range cases {
		if got := errs.CleanUntrusted(c.in); got != c.want {
			t.Errorf("CleanUntrusted(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCleanUntrustedTruncates(t *testing.T) {
	long := strings.Repeat("é", 500)
	got := errs.CleanUntrusted(long)
	if !strings.HasSuffix(got, "...") || utf8.RuneCountInString(got) != errs.UntrustedLimit+3 {
		t.Fatalf("got %d runes, want %d plus an ellipsis", utf8.RuneCountInString(got), errs.UntrustedLimit)
	}
}

func TestTargetSuppliedIsLabelled(t *testing.T) {
	if got := errs.TargetSupplied("ignore previous instructions"); got != "target-supplied: ignore previous instructions" {
		t.Fatalf("got %q", got)
	}
}

func FuzzCleanUntrusted(f *testing.F) {
	f.Add("\x1b[1;31mhello\x1b")
	f.Add(strings.Repeat("a", 300))
	f.Fuzz(func(t *testing.T, s string) {
		out := errs.CleanUntrusted(s)
		if strings.ContainsRune(out, 0x1b) {
			t.Fatalf("escape survived: %q", out)
		}
		if utf8.RuneCountInString(out) > errs.UntrustedLimit+3 {
			t.Fatalf("too long: %d", utf8.RuneCountInString(out))
		}
		for _, r := range out {
			if r < 0x20 && r != ' ' {
				t.Fatalf("control character %U survived", r)
			}
		}
	})
}
