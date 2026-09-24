package guard_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/adapters/guard"
)

// The request checks are exercised end to end in the REST suite.

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestGuardConstruction(t *testing.T) {
	if _, err := guard.New("127.0.0.1:1", guard.Options{Token: "short"}); err == nil {
		t.Fatal("a short token was accepted")
	}
	if err := guard.CheckBind("0.0.0.0:7474", false); err == nil {
		t.Fatal("a non-loopback bind was allowed by default")
	}
	if err := guard.CheckBind("0.0.0.0:7474", true); err != nil {
		t.Fatal(err)
	}
	if err := guard.CheckBind("127.0.0.1:7474", false); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "sub", ".token")
	tok, path, err := guard.Token("", file)
	if err != nil || len(tok) < 32 || path != file {
		t.Fatalf("generated token: %q %q %v", tok, path, err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode: %v %v", fi.Mode(), err)
	}
	if again, _, err := guard.Token("", file); err != nil || again != tok {
		t.Fatal("the token file was not reused")
	}
	if given, path, _ := guard.Token("given-token-0123456", file); given != "given-token-0123456" || path != "" {
		t.Fatal("a given token was not used")
	}
}
