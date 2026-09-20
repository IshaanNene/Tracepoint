// Package policy models the safety envelope a human grants.
//
// Two documents with different authority. A *policy* is granted by a person, through
// --policy, TRACEPOINT_POLICY or flags, and is fixed for the life of a server. A
// *configuration* is written by whoever is running the test, which may be an agent.
// A configuration can only ever tighten a policy; asking for more is refused, with a
// message naming exactly what a human would have to change.
//
// Refused, not clamped. Clamping would let a run proceed while quietly measuring
// something other than what was asked for, which is worse than not running at all.
// There are no interactive confirmations either: a prompt is not a control when stdin
// is a pipe. See docs/adr/006-policy-and-threat-model.md.
package policy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Version is the only policy format version that exists.
const Version = 1

// EnvVar names a policy file when no flag is given.
const EnvVar = "TRACEPOINT_POLICY"

// Policy is the envelope.
type Policy struct {
	Version            int      `yaml:"version" json:"version"`
	AllowTargets       []string `yaml:"allow_targets" json:"allow_targets,omitempty"`
	DenyTargets        []string `yaml:"deny_targets" json:"deny_targets,omitempty"`
	AllowPublicTargets bool     `yaml:"allow_public_targets" json:"allow_public_targets"`
	AllowWrites        bool     `yaml:"allow_writes" json:"allow_writes"`
	AllowDangerous     bool     `yaml:"allow_dangerous" json:"allow_dangerous"`
	AllowInsecureTLS   bool     `yaml:"allow_insecure_tls" json:"allow_insecure_tls"`
	AllowDetach        bool     `yaml:"allow_detach" json:"allow_detach"`
	MaxRatePerRunner   *float64 `yaml:"max_rate_per_runner" json:"max_rate_per_runner,omitempty"`
	MaxInFlight        *int     `yaml:"max_in_flight" json:"max_in_flight,omitempty"`
	MaxDuration        *string  `yaml:"max_duration" json:"max_duration,omitempty"`
	MaxConcurrentRuns  int      `yaml:"max_concurrent_runs" json:"max_concurrent_runs"`
	RunRoot            string   `yaml:"run_root" json:"run_root"`
	RedactHeaders      []string `yaml:"redact_headers" json:"redact_headers,omitempty"`
}

// Default is the envelope a person gets at their own terminal.
//
// Permissive about how much load, because someone typing a command has already decided
// to run a load test; strict about *where*, because pointing a load generator at an
// arbitrary public host is how a test becomes an attack, and about writes, because a
// mistyped DSN should not be able to modify anything.
func Default() Policy {
	return Policy{
		Version:           Version,
		AllowDetach:       true,
		MaxConcurrentRuns: 1,
		RunRoot:           "runs",
	}
}

// ServerDefault is the envelope an MCP or REST server starts with when no policy file
// is supplied. Deliberately tighter than Default: the caller is a program, and nobody
// is necessarily watching (spec §6.5).
func ServerDefault() Policy {
	p := Default()
	rate := 500.0
	inFlight := 512
	duration := "10m"
	p.MaxRatePerRunner = &rate
	p.MaxInFlight = &inFlight
	p.MaxDuration = &duration
	return p
}

// MaxRunDuration returns the configured ceiling, if any.
func (p Policy) MaxRunDuration() (time.Duration, bool, error) {
	if p.MaxDuration == nil || *p.MaxDuration == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(*p.MaxDuration)
	if err != nil {
		return 0, false, errs.Wrap(errs.CodePolicyParse, err,
			"max_duration %q is not a duration", *p.MaxDuration).
			WithHint("use a Go duration such as 10m")
	}
	return d, true, nil
}

// Load reads a policy file. An empty path returns the default envelope, so a caller
// never has to branch on whether one was supplied.
func Load(path string, lookupEnv func(string) (string, bool)) (Policy, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if path == "" {
		if v, ok := lookupEnv(EnvVar); ok && v != "" {
			path = v
		}
	}
	if path == "" {
		return Default(), nil
	}

	// Reading a path the operator supplied is the entire purpose of this function.
	raw, err := os.ReadFile(path) //nolint:gosec // the policy file is named by a human, deliberately
	if err != nil {
		return Policy{}, errs.Wrap(errs.CodePolicyParse, err, "reading the policy %s", path).
			WithHint("check the path, or omit --policy to use the default envelope")
	}

	p := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Policy{}, errs.Wrap(errs.CodePolicyParse, err, "%s is not a valid policy", path).
			WithHint("see `tracepoint schema policy`")
	}
	if p.Version != Version {
		return Policy{}, errs.New(errs.CodePolicyParse,
			"unsupported policy version %d", p.Version).
			WithPath("/version").
			WithHint("this build understands version %d", Version)
	}
	if _, _, err := p.MaxRunDuration(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Tightening is what a configuration's safety section asks for. Every field is what
// the configuration wants; Tighten decides whether the policy permits it.
type Tightening struct {
	AllowWrites      bool
	AllowDangerous   bool
	AllowInsecureTLS bool
	AllowTargets     []string
	MaxRatePerRunner *float64
	MaxInFlight      *int
	MaxDuration      *time.Duration
}

// Tighten applies a configuration's safety section to the policy.
//
// Booleans are conjunctions: a capability is available only when both the human's
// policy and the configuration ask for it. That means writes need an explicit
// allow_writes in *both* - the person granting the envelope and the person writing the
// test each have to say so - and a configuration that simply does not mention writes
// gets none, which is the safe reading of silence.
//
// Numeric limits take the smaller of the two. Asking for a larger one is a refusal
// rather than a clamp.
func (p Policy) Tighten(t Tightening) (Policy, error) {
	out := p
	var problems []*errs.Error

	if t.AllowWrites && !p.AllowWrites {
		problems = append(problems, errs.New(errs.CodePolicyWritesNotAllowed,
			"the configuration asks to perform writes, which the policy does not grant").
			WithPath("/safety/allow_writes").
			WithHint("a human must add allow_writes: true to the policy file"))
	}
	out.AllowWrites = p.AllowWrites && t.AllowWrites

	if t.AllowDangerous && !p.AllowDangerous {
		problems = append(problems, errs.New(errs.CodePolicyDangerousNotAllowed,
			"the configuration asks to run destructive statements, which the policy does not grant").
			WithPath("/safety/allow_dangerous").
			WithHint("a human must add allow_dangerous: true to the policy file"))
	}
	out.AllowDangerous = p.AllowDangerous && t.AllowDangerous

	if t.AllowInsecureTLS && !p.AllowInsecureTLS {
		problems = append(problems, errs.New(errs.CodePolicyInsecureTLS,
			"the configuration asks to skip certificate verification, which the policy does not grant").
			WithPath("/safety/allow_insecure_tls").
			WithHint("a human must add allow_insecure_tls: true to the policy file, or supply a CA bundle instead"))
	}
	out.AllowInsecureTLS = p.AllowInsecureTLS && t.AllowInsecureTLS

	// A configuration may narrow the allowlist but not extend it. An entry the policy
	// does not already permit is a refusal naming that entry, rather than a target
	// that mysteriously fails later.
	if len(t.AllowTargets) > 0 {
		var unpermitted []string
		for _, entry := range t.AllowTargets {
			if !p.allowlistPermits(entry) {
				unpermitted = append(unpermitted, entry)
			}
		}
		if len(unpermitted) > 0 {
			problems = append(problems, errs.New(errs.CodePolicyTargetNotAllowed,
				"the configuration allowlists %s, which the policy does not", strings.Join(unpermitted, ", ")).
				WithPath("/safety/allow_targets").
				WithHint("a human must add %s to allow_targets in the policy file", strings.Join(unpermitted, ", ")))
		}
		out.AllowTargets = t.AllowTargets
	}

	if t.MaxRatePerRunner != nil {
		if p.MaxRatePerRunner != nil && *t.MaxRatePerRunner > *p.MaxRatePerRunner {
			problems = append(problems, exceeds("max_rate_per_runner",
				fmt.Sprintf("%g", *t.MaxRatePerRunner), fmt.Sprintf("%g", *p.MaxRatePerRunner)))
		} else {
			v := *t.MaxRatePerRunner
			out.MaxRatePerRunner = &v
		}
	}
	if t.MaxInFlight != nil {
		if p.MaxInFlight != nil && *t.MaxInFlight > *p.MaxInFlight {
			problems = append(problems, exceeds("max_in_flight",
				fmt.Sprintf("%d", *t.MaxInFlight), fmt.Sprintf("%d", *p.MaxInFlight)))
		} else {
			v := *t.MaxInFlight
			out.MaxInFlight = &v
		}
	}
	if t.MaxDuration != nil {
		limit, has, err := p.MaxRunDuration()
		if err != nil {
			return Policy{}, err
		}
		if has && *t.MaxDuration > limit {
			problems = append(problems, exceeds("max_duration", t.MaxDuration.String(), limit.String()))
		} else {
			v := t.MaxDuration.String()
			out.MaxDuration = &v
		}
	}

	if len(problems) > 0 {
		return Policy{}, summarise(problems)
	}
	return out, nil
}

func exceeds(field, want, limit string) *errs.Error {
	return errs.New(errs.CodePolicyDenied,
		"the configuration sets %s to %s, above the policy limit of %s", field, want, limit).
		WithPath("/safety/"+field).
		WithHint("lower it to %s or below, or a human must raise the policy", limit)
}

func summarise(list []*errs.Error) error {
	if len(list) == 1 {
		return list[0]
	}
	head := errs.New(errs.CodePolicyDenied, "the configuration asks for %d things the policy does not grant", len(list))
	return head.WithCauses(list...)
}

// CheckRate refuses an offered rate above the policy's ceiling.
func (p Policy) CheckRate(runner string, rate float64) error {
	if p.MaxRatePerRunner == nil || rate <= *p.MaxRatePerRunner {
		return nil
	}
	return errs.New(errs.CodePolicyRateExceeded,
		"the %s runner offers %g operations per second, above the policy limit of %g",
		runner, rate, *p.MaxRatePerRunner).
		WithPath("/"+runner+"/executor").
		WithHint("lower the rate to %g or below, or a human must raise max_rate_per_runner in the policy",
			*p.MaxRatePerRunner)
}

// CheckInFlight refuses a concurrency ceiling above the policy's.
func (p Policy) CheckInFlight(runner string, inFlight int) error {
	if p.MaxInFlight == nil || inFlight <= *p.MaxInFlight {
		return nil
	}
	return errs.New(errs.CodePolicyDenied,
		"the %s runner allows %d operations in flight, above the policy limit of %d",
		runner, inFlight, *p.MaxInFlight).
		WithPath("/"+runner+"/executor/max_in_flight").
		WithHint("lower it to %d or below, or a human must raise max_in_flight in the policy", *p.MaxInFlight)
}

// CheckDuration refuses a run longer than the policy allows.
func (p Policy) CheckDuration(d time.Duration) error {
	limit, has, err := p.MaxRunDuration()
	if err != nil {
		return err
	}
	if !has || d <= limit {
		return nil
	}
	return errs.New(errs.CodePolicyDurationExceeded,
		"the run lasts %s, above the policy limit of %s", d, limit).
		WithPath("/run/duration").
		WithHint("shorten it to %s or less, or a human must raise max_duration in the policy", limit)
}

// CheckWrites refuses write traffic the policy has not granted.
func (p Policy) CheckWrites(runner string, labels []string) error {
	if p.AllowWrites || len(labels) == 0 {
		return nil
	}
	sort.Strings(labels)
	return errs.New(errs.CodePolicyWritesNotAllowed,
		"the %s runner performs writes (%s) but writes are not allowed", runner, strings.Join(labels, ", ")).
		WithPath("/" + runner).
		WithHint("set safety.allow_writes: true in the configuration and have a human grant allow_writes in the policy, or mark these read-only")
}

// CheckDangerous refuses destructive or administrative statements.
func (p Policy) CheckDangerous(runner string, found []Dangerous) error {
	if len(found) == 0 {
		return nil
	}
	if p.AllowDangerous {
		return nil
	}
	parts := make([]string, 0, len(found))
	for _, d := range found {
		parts = append(parts, fmt.Sprintf("%s uses %s", d.Label, d.Keyword))
	}
	sort.Strings(parts)
	return errs.New(errs.CodePolicyDangerousNotAllowed,
		"the %s runner contains destructive statements: %s", runner, strings.Join(parts, "; ")).
		WithPath("/" + runner).
		WithHint("these change or destroy data irreversibly; if that is genuinely intended, a human must grant allow_dangerous in the policy")
}

// Dangerous names one destructive statement found in a configuration.
type Dangerous struct {
	Label   string
	Keyword string
}

// Scope classifies a resolved address.
type Scope string

// Address scopes.
const (
	ScopeLoopback  Scope = "loopback"
	ScopePrivate   Scope = "private"
	ScopeLinkLocal Scope = "link-local"
	ScopePublic    Scope = "public"
)

// Target is a resolved destination and what the policy makes of it.
type Target struct {
	Host  string
	Addrs []string
	Scope Scope
}

// ClassifyAddrs reports the widest scope among a host's addresses.
//
// Widest, deliberately: a host that resolves to both a private and a public address is
// reachable publicly, and a safety decision has to be made on the worst case rather
// than on whichever address happened to come back first.
func ClassifyAddrs(addrs []string) Scope {
	rank := map[Scope]int{ScopeLoopback: 0, ScopeLinkLocal: 1, ScopePrivate: 2, ScopePublic: 3}
	widest := ScopeLoopback
	for _, a := range addrs {
		s := classifyOne(a)
		if rank[s] > rank[widest] {
			widest = s
		}
	}
	return widest
}

func classifyOne(addr string) Scope {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		// An address that will not parse cannot be shown to be safe.
		return ScopePublic
	}
	switch {
	case ip.IsLoopback():
		return ScopeLoopback
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return ScopeLinkLocal
	case ip.IsPrivate(), ip.IsUnspecified():
		return ScopePrivate
	default:
		return ScopePublic
	}
}

// CheckTarget decides whether a resolved host may be driven.
//
// Loopback, private and link-local pass without listing, because those are the
// networks a person testing their own system is on. A public address is refused unless
// a human listed it, and the refusal says exactly what to add and where.
func (p Policy) CheckTarget(t Target) error {
	for _, entry := range p.DenyTargets {
		if matches(entry, t) {
			return errs.New(errs.CodePolicyTargetNotAllowed,
				"%s is on the policy's deny list", t.Host).
				WithHint("deny_targets is checked first and overrides every allowance").
				WithDetail("host", t.Host)
		}
	}
	if t.Scope != ScopePublic {
		return nil
	}
	if p.AllowPublicTargets {
		return nil
	}
	for _, entry := range p.AllowTargets {
		if matches(entry, t) {
			return nil
		}
	}
	return errs.New(errs.CodePolicyTargetNotAllowed,
		"%s resolves to a public address (%s) and is not allowlisted",
		t.Host, strings.Join(t.Addrs, ", ")).
		WithHint("a human must add %q to allow_targets in the policy file; TracePoint will not point load at an arbitrary public host", t.Host).
		WithDetail("host", t.Host).
		WithDetail("addrs", t.Addrs)
}

// allowlistPermits reports whether the policy's own allowlist covers an entry a
// configuration wants to keep.
func (p Policy) allowlistPermits(entry string) bool {
	if p.AllowPublicTargets {
		return true
	}
	for _, permitted := range p.AllowTargets {
		if strings.EqualFold(permitted, entry) {
			return true
		}
		// A CIDR in the policy covers an address inside it.
		if prefix, err := netip.ParsePrefix(permitted); err == nil {
			if ip, err := netip.ParseAddr(entry); err == nil && prefix.Contains(ip) {
				return true
			}
		}
	}
	// An entry that is not public needs no permission in the first place.
	if ip, err := netip.ParseAddr(entry); err == nil {
		return classifyOne(ip.String()) != ScopePublic
	}
	return false
}

// matches reports whether an allowlist or denylist entry covers a target. An entry may
// be a hostname, an IP address or a CIDR.
func matches(entry string, t Target) bool {
	if strings.EqualFold(entry, t.Host) {
		return true
	}
	if prefix, err := netip.ParsePrefix(entry); err == nil {
		for _, a := range t.Addrs {
			if ip, err := netip.ParseAddr(a); err == nil && prefix.Contains(ip) {
				return true
			}
		}
		return false
	}
	if _, err := netip.ParseAddr(entry); err == nil {
		for _, a := range t.Addrs {
			if strings.EqualFold(entry, a) {
				return true
			}
		}
	}
	return false
}

// Resolve looks a host up and classifies what it resolved to.
//
// Resolution happens before any load, and the classification is made on the addresses
// rather than on the name, because a name says nothing about where it points: an
// innocuous-looking internal hostname can resolve straight to the public internet.
func Resolve(ctx context.Context, resolver *net.Resolver, host string) (Target, error) {
	t := Target{Host: host}

	// A literal address needs no lookup, and asking a resolver for one would be a
	// pointless round trip on every preflight.
	if ip, err := netip.ParseAddr(host); err == nil {
		t.Addrs = []string{ip.String()}
		t.Scope = ClassifyAddrs(t.Addrs)
		return t, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return t, errs.Wrap(errs.CodePreflightDNS, err, "%s does not resolve", host).
			WithHint("check the hostname, and that this machine can reach the name server it needs")
	}
	t.Addrs = addrs
	t.Scope = ClassifyAddrs(addrs)
	return t, nil
}
