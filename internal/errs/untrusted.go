package errs

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// UntrustedLimit is the most characters of a target-supplied string that TracePoint
// will repeat anywhere (§6.5).
const UntrustedLimit = 200

// TargetSupplied renders a string that came from a target - a server's error message,
// a header - so that it is safe to hand to a downstream reader, including an LLM.
//
// The target is untrusted input: whoever controls it controls this text. So the text is
// stripped of ANSI escapes and control characters, truncated to UntrustedLimit
// characters, and labelled as target-supplied, so a reader can never mistake it for
// something TracePoint itself concluded.
func TargetSupplied(s string) string {
	return "target-supplied: " + CleanUntrusted(s)
}

// CleanUntrusted strips escapes and control characters and truncates, without the
// label, for places whose field name already says where the text came from.
func CleanUntrusted(s string) string {
	var b strings.Builder
	b.Grow(min(len(s), UntrustedLimit*4))
	n := 0
	for i := 0; i < len(s) && n < UntrustedLimit; {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == 0x1b:
			// Skip an ANSI escape: ESC, an optional '[' with parameters, and a final
			// letter. Anything else following ESC is dropped with it.
			i += size
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
			}
			if i < len(s) {
				i++
			}
			continue
		case r == utf8.RuneError && size == 1:
			i++
			continue
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			i += size
			continue
		default:
			b.WriteRune(r)
		}
		n++
		i += size
	}
	out := strings.TrimSpace(b.String())
	if n >= UntrustedLimit {
		out += "..."
	}
	return out
}
