package policy_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func ptr[T any](v T) *T { return &v }

func noEnv(string) (string, bool) { return "", false }

func TestDefaultsAreSafe(t *testing.T) {
	t.Parallel()
	p := policy.Default()
	if p.AllowWrites || p.AllowDangerous || p.AllowPublicTargets || p.AllowInsecureTLS {
		t.Errorf("the default envelope grants something it should not: %+v", p)
	}
	if p.MaxConcurrentRuns != 1 {
		t.Errorf("max_concurrent_runs = %d, want 1; two tests against one target measure each other", p.MaxConcurrentRuns)
	}

	// A server is driven by a program with nobody necessarily watching, so its
	// defaults are tighter than a person's terminal.
	s := policy.ServerDefault()
	if s.MaxRatePerRunner == nil || s.MaxInFlight == nil || s.MaxDuration == nil {
		t.Errorf("the server envelope should cap rate, concurrency and duration: %+v", s)
	}
	if s.AllowWrites || s.AllowPublicTargets {
		t.Error("the server envelope must not grant writes or public targets")
	}
}

// Loopback, private and link-local are the networks someone testing their own system
// is on, and need no listing.
func TestPrivateTargetsNeedNoAllowlisting(t *testing.T) {
	t.Parallel()
	p := policy.Default()
	for _, tc := range []struct {
		host  string
		addrs []string
		scope policy.Scope
	}{
		{"localhost", []string{"127.0.0.1"}, policy.ScopeLoopback},
		{"::1", []string{"::1"}, policy.ScopeLoopback},
		{"db.internal", []string{"10.0.3.4"}, policy.ScopePrivate},
		{"svc", []string{"192.168.1.10"}, policy.ScopePrivate},
		{"svc", []string{"172.16.0.1"}, policy.ScopePrivate},
		{"link", []string{"169.254.1.1"}, policy.ScopeLinkLocal},
	} {
		t.Run(tc.host+"/"+tc.addrs[0], func(t *testing.T) {
			if got := policy.ClassifyAddrs(tc.addrs); got != tc.scope {
				t.Errorf("ClassifyAddrs(%v) = %s, want %s", tc.addrs, got, tc.scope)
			}
			if err := p.CheckTarget(policy.Target{Host: tc.host, Addrs: tc.addrs, Scope: tc.scope}); err != nil {
				t.Errorf("CheckTarget: %v", err)
			}
		})
	}
}

// Pointing a load generator at an arbitrary public host is how a test becomes an
// attack, so it is refused unless a human listed it - and the refusal must say so.
func TestPublicTargetsAreRefusedUnlessAllowlisted(t *testing.T) {
	t.Parallel()
	target := policy.Target{Host: "api.example.com", Addrs: []string{"93.184.216.34"}, Scope: policy.ScopePublic}

	err := policy.Default().CheckTarget(target)
	if err == nil {
		t.Fatal("a public target was allowed without being listed")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodePolicyTargetNotAllowed {
		t.Fatalf("code = %v, want %s", err, errs.CodePolicyTargetNotAllowed)
	}
	if !strings.Contains(typed.Hint, "allow_targets") || !strings.Contains(typed.Hint, "api.example.com") {
		t.Errorf("hint = %q, want it to name the host and the policy field a human must change", typed.Hint)
	}
	if typed.ExitCode() != errs.ExitUsage {
		t.Errorf("exit code = %d, want %d", typed.ExitCode(), errs.ExitUsage)
	}

	t.Run("by hostname", func(t *testing.T) {
		p := policy.Default()
		p.AllowTargets = []string{"api.example.com"}
		if err := p.CheckTarget(target); err != nil {
			t.Errorf("an allowlisted host was refused: %v", err)
		}
	})
	t.Run("by address", func(t *testing.T) {
		p := policy.Default()
		p.AllowTargets = []string{"93.184.216.34"}
		if err := p.CheckTarget(target); err != nil {
			t.Errorf("an allowlisted address was refused: %v", err)
		}
	})
	t.Run("by CIDR", func(t *testing.T) {
		p := policy.Default()
		p.AllowTargets = []string{"93.184.216.0/24"}
		if err := p.CheckTarget(target); err != nil {
			t.Errorf("an allowlisted CIDR was refused: %v", err)
		}
	})
	t.Run("a different host is still refused", func(t *testing.T) {
		p := policy.Default()
		p.AllowTargets = []string{"other.example.com"}
		if err := p.CheckTarget(target); err == nil {
			t.Error("allowlisting one host allowed another")
		}
	})
	t.Run("allow_public_targets opens it", func(t *testing.T) {
		p := policy.Default()
		p.AllowPublicTargets = true
		if err := p.CheckTarget(target); err != nil {
			t.Errorf("CheckTarget: %v", err)
		}
	})
}

// The denylist is checked first and beats everything, so an operator can carve out
// production even inside a permissive envelope.
func TestDenyListOverridesEveryAllowance(t *testing.T) {
	t.Parallel()
	p := policy.Default()
	p.AllowPublicTargets = true
	p.AllowTargets = []string{"prod-db.internal", "10.0.0.0/8"}
	p.DenyTargets = []string{"prod-db.internal"}

	err := p.CheckTarget(policy.Target{Host: "prod-db.internal", Addrs: []string{"10.0.0.5"}, Scope: policy.ScopePrivate})
	if err == nil {
		t.Fatal("a denied host was allowed")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodePolicyTargetNotAllowed {
		t.Fatalf("code = %v", err)
	}

	// A denied CIDR covers the addresses inside it, even for a private host.
	p2 := policy.Default()
	p2.DenyTargets = []string{"10.0.0.0/8"}
	if err := p2.CheckTarget(policy.Target{Host: "svc", Addrs: []string{"10.1.2.3"}, Scope: policy.ScopePrivate}); err == nil {
		t.Error("a denied CIDR did not cover an address inside it")
	}
}

// A host that resolves to both a private and a public address is reachable publicly,
// and the decision has to be made on the worst case.
func TestMixedResolutionTakesTheWidestScope(t *testing.T) {
	t.Parallel()
	if got := policy.ClassifyAddrs([]string{"10.0.0.1", "93.184.216.34"}); got != policy.ScopePublic {
		t.Errorf("ClassifyAddrs = %s, want public: the host is reachable publicly", got)
	}
	if got := policy.ClassifyAddrs([]string{"127.0.0.1", "10.0.0.1"}); got != policy.ScopePrivate {
		t.Errorf("ClassifyAddrs = %s, want private", got)
	}
	// An address that will not parse cannot be shown to be safe.
	if got := policy.ClassifyAddrs([]string{"not-an-address"}); got != policy.ScopePublic {
		t.Errorf("ClassifyAddrs = %s, want public for an unparseable address", got)
	}
}

func TestTighteningCannotWiden(t *testing.T) {
	t.Parallel()
	strict := policy.Default()

	cases := []struct {
		name string
		t    policy.Tightening
		code errs.Code
	}{
		{"writes", policy.Tightening{AllowWrites: true}, errs.CodePolicyWritesNotAllowed},
		{"dangerous", policy.Tightening{AllowDangerous: true}, errs.CodePolicyDangerousNotAllowed},
		{"insecure tls", policy.Tightening{AllowInsecureTLS: true}, errs.CodePolicyInsecureTLS},
		{"an unlisted target", policy.Tightening{AllowTargets: []string{"api.example.com"}}, errs.CodePolicyTargetNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := strict.Tighten(tc.t)
			if err == nil {
				t.Fatalf("a configuration widened the policy (%s) without being refused", tc.name)
			}
			var typed *errs.Error
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("code = %v, want %s", err, tc.code)
			}
			if !strings.Contains(typed.Hint, "policy") {
				t.Errorf("hint = %q, want it to say what a human must change in the policy", typed.Hint)
			}
		})
	}
}

func TestTighteningNarrowsFreely(t *testing.T) {
	t.Parallel()
	permissive := policy.Default()
	permissive.AllowWrites = true
	permissive.AllowDangerous = true
	permissive.AllowTargets = []string{"a.example.com", "b.example.com"}
	permissive.MaxRatePerRunner = ptr(1000.0)
	permissive.MaxInFlight = ptr(512)
	permissive.MaxDuration = ptr("1h")

	got, err := permissive.Tighten(policy.Tightening{
		AllowWrites:      true,
		AllowDangerous:   false, // deliberately narrower
		AllowTargets:     []string{"a.example.com"},
		MaxRatePerRunner: ptr(100.0),
		MaxInFlight:      ptr(32),
		MaxDuration:      ptr(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Tighten: %v", err)
	}
	if !got.AllowWrites {
		t.Error("writes granted by both should be available")
	}
	if got.AllowDangerous {
		t.Error("a configuration declining a capability must lose it, even when the policy grants it")
	}
	if len(got.AllowTargets) != 1 || got.AllowTargets[0] != "a.example.com" {
		t.Errorf("allowlist = %v, want it narrowed to the one entry", got.AllowTargets)
	}
	if *got.MaxRatePerRunner != 100 || *got.MaxInFlight != 32 {
		t.Errorf("limits were not narrowed: %v %v", *got.MaxRatePerRunner, *got.MaxInFlight)
	}
	if d, _, _ := got.MaxRunDuration(); d != 5*time.Minute {
		t.Errorf("max duration = %v, want 5m", d)
	}
}

// Writes need both the human's policy and the configuration to say so. Silence in the
// configuration means no writes, which is the safe reading.
func TestWritesNeedBothPolicyAndConfiguration(t *testing.T) {
	t.Parallel()
	permissive := policy.Default()
	permissive.AllowWrites = true

	silent, err := permissive.Tighten(policy.Tightening{})
	if err != nil {
		t.Fatalf("Tighten: %v", err)
	}
	if silent.AllowWrites {
		t.Error("a configuration that says nothing about writes must not get them")
	}
	if err := silent.CheckWrites("db", []string{"insert-item"}); err == nil {
		t.Error("a write was allowed although the configuration never asked for one")
	}
}

func TestLimitsAreEnforced(t *testing.T) {
	t.Parallel()
	p := policy.Default()
	p.MaxRatePerRunner = ptr(100.0)
	p.MaxInFlight = ptr(64)
	p.MaxDuration = ptr("10m")

	if err := p.CheckRate("http", 100); err != nil {
		t.Errorf("a rate at the limit was refused: %v", err)
	}
	err := p.CheckRate("http", 101)
	if err == nil {
		t.Fatal("a rate above the limit was allowed")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodePolicyRateExceeded {
		t.Errorf("code = %v, want %s", err, errs.CodePolicyRateExceeded)
	}

	if err := p.CheckInFlight("http", 65); err == nil {
		t.Error("concurrency above the limit was allowed")
	}
	if err := p.CheckDuration(11 * time.Minute); err == nil {
		t.Error("a run longer than the limit was allowed")
	}
	if err := p.CheckDuration(10 * time.Minute); err != nil {
		t.Errorf("a run at the limit was refused: %v", err)
	}

	// No ceiling configured means no ceiling enforced.
	open := policy.Default()
	if err := open.CheckRate("http", 1e6); err != nil {
		t.Errorf("an unlimited policy refused a rate: %v", err)
	}
}

func TestDangerousStatementsAreRefused(t *testing.T) {
	t.Parallel()
	found := []policy.Dangerous{{Label: "wipe", Keyword: "TRUNCATE"}}

	err := policy.Default().CheckDangerous("db", found)
	if err == nil {
		t.Fatal("a destructive statement was allowed")
	}
	var typed *errs.Error
	if !errors.As(err, &typed) || typed.Code != errs.CodePolicyDangerousNotAllowed {
		t.Fatalf("code = %v", err)
	}
	if !strings.Contains(typed.Message, "TRUNCATE") || !strings.Contains(typed.Message, "wipe") {
		t.Errorf("message = %q, want it to name the statement and its label", typed.Message)
	}

	granted := policy.Default()
	granted.AllowDangerous = true
	if err := granted.CheckDangerous("db", found); err != nil {
		t.Errorf("a granted destructive statement was refused: %v", err)
	}
	if err := policy.Default().CheckDangerous("db", nil); err != nil {
		t.Errorf("nothing dangerous was found but it was refused anyway: %v", err)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("no path gives the default envelope", func(t *testing.T) {
		p, err := policy.Load("", noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if p.AllowWrites {
			t.Error("the default envelope should not grant writes")
		}
	})

	t.Run("a file is read", func(t *testing.T) {
		path := filepath.Join(dir, "policy.yaml")
		body := "version: 1\nallow_writes: true\nallow_targets: [\"api.staging.internal\"]\nmax_duration: 10m\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing: %v", err)
		}
		p, err := policy.Load(path, noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !p.AllowWrites || len(p.AllowTargets) != 1 {
			t.Errorf("loaded %+v", p)
		}
		if d, has, _ := p.MaxRunDuration(); !has || d != 10*time.Minute {
			t.Errorf("max duration = %v", d)
		}
	})

	t.Run("the environment variable is used when no path is given", func(t *testing.T) {
		path := filepath.Join(dir, "fromenv.yaml")
		if err := os.WriteFile(path, []byte("version: 1\nallow_dangerous: true\n"), 0o600); err != nil {
			t.Fatalf("writing: %v", err)
		}
		p, err := policy.Load("", func(k string) (string, bool) {
			if k == policy.EnvVar {
				return path, true
			}
			return "", false
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !p.AllowDangerous {
			t.Error("the policy named by the environment was not loaded")
		}
	})

	t.Run("errors", func(t *testing.T) {
		for name, body := range map[string]string{
			"unknown field":      "version: 1\nallow_evrything: true\n",
			"wrong version":      "version: 2\n",
			"malformed duration": "version: 1\nmax_duration: soon\n",
			"not yaml":           "version: 1\n\tbad: [\n",
		} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "p.yaml")
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatalf("writing: %v", err)
				}
				if _, err := policy.Load(path, noEnv); err == nil {
					t.Errorf("%s was accepted", name)
				}
			})
		}
		if _, err := policy.Load(filepath.Join(dir, "absent.yaml"), noEnv); err == nil {
			t.Error("a missing policy file was accepted")
		}
	})
}

func TestResolveLiteralAddressSkipsTheResolver(t *testing.T) {
	t.Parallel()
	got, err := policy.Resolve(t.Context(), nil, "127.0.0.1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Scope != policy.ScopeLoopback || len(got.Addrs) != 1 {
		t.Errorf("Resolve(127.0.0.1) = %+v", got)
	}
}
