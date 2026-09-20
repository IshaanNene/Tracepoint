package config

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
)

// Warning is a non-fatal observation made while applying a policy.
type Warning struct {
	Code    string
	Message string
	Fix     string
}

// ApplyPolicy resolves the effective envelope for this configuration and refuses
// anything it is not allowed to do.
//
// Order matters. The configuration's safety section tightens the policy first, so every
// later check is made against the envelope that will actually be in force. Then the
// statements are classified and the write and destructive guards applied, then the
// numeric ceilings. Targets are checked later, at preflight, because they need DNS.
//
// Everything that is refused is refused here, before a single operation is performed.
func (c *Config) ApplyPolicy(granted policy.Policy) (policy.Policy, []Warning, error) {
	effective, err := granted.Tighten(c.tightening())
	if err != nil {
		return policy.Policy{}, nil, err
	}

	writes, dangerous, warnings := c.classifyStatements()

	var problems []*errs.Error
	for _, runner := range []string{"db", "redis"} {
		if err := effective.CheckWrites(runner, writes[runner]); err != nil {
			problems = append(problems, asCoded(err))
		}
		if err := effective.CheckDangerous(runner, dangerous[runner]); err != nil {
			problems = append(problems, asCoded(err))
		}
	}

	if err := effective.CheckDuration(c.Run.Duration.D()); err != nil {
		problems = append(problems, asCoded(err))
	}
	for _, name := range c.Runners() {
		ex := c.Executor(name)
		if ex == nil {
			continue
		}
		if err := effective.CheckRate(name, peakRate(ex)); err != nil {
			problems = append(problems, asCoded(err))
		}
		if ex.MaxInFlight > 0 {
			if err := effective.CheckInFlight(name, ex.MaxInFlight); err != nil {
				problems = append(problems, asCoded(err))
			}
		}
	}
	if err := c.checkInsecureTLS(effective); err != nil {
		problems = append(problems, asCoded(err))
	}

	if len(problems) > 0 {
		return policy.Policy{}, nil, summarise(problems)
	}
	return effective, warnings, nil
}

func (c *Config) tightening() policy.Tightening {
	t := policy.Tightening{
		AllowWrites:      c.Safety.AllowWrites,
		AllowDangerous:   c.Safety.AllowDangerous,
		AllowInsecureTLS: c.Safety.AllowInsecureTLS,
		AllowTargets:     c.Safety.AllowTargets,
		MaxRatePerRunner: c.Safety.MaxRatePerRunner,
		MaxInFlight:      c.Safety.MaxInFlight,
	}
	if c.Safety.MaxDuration != nil {
		d := c.Safety.MaxDuration.D()
		t.MaxDuration = &d
	}
	return t
}

// PeakRate is the highest rate a profile ever offers, which is what a ceiling has to
// be measured against.
func PeakRate(e *Executor) float64 { return peakRate(e) }

// ClassifyForPlan reports what each runner's statements do, for the dry-run plan. It
// makes no policy decision; it only describes.
func (c *Config) ClassifyForPlan() (writes map[string][]string, dangerous map[string][]policy.Dangerous, warnings []Warning) {
	return c.classifyStatements()
}

// peakRate is the highest rate the profile ever offers, which is what a ceiling has to
// be measured against - an average would let a configuration ramp far above the limit
// as long as it came back down.
func peakRate(e *Executor) float64 {
	peak := e.Rate
	for _, s := range e.Stages {
		if s.Target > peak {
			peak = s.Target
		}
	}
	return peak
}

// classifyStatements works out what each runner's statements actually do.
//
// The configuration's declared `type: read|write` is authoritative where it is given,
// because the author knows and the parser is guessing. Where it is absent the
// classifier decides, and anything it does not recognise counts as a write.
func (c *Config) classifyStatements() (writes map[string][]string, dangerous map[string][]policy.Dangerous, warnings []Warning) {
	writes = map[string][]string{}
	dangerous = map[string][]policy.Dangerous{}

	if c.DB != nil {
		for _, q := range c.DB.Queries {
			statements := make([]string, 0, len(q.Tx)+1)
			if q.SQL != "" {
				statements = append(statements, q.SQL)
			}
			for _, tx := range q.Tx {
				statements = append(statements, tx.SQL)
			}

			isWrite := q.Type == "write"
			for _, sql := range statements {
				got := policy.ClassifySQL(sql)
				if got.Dangerous != "" {
					dangerous["db"] = append(dangerous["db"], policy.Dangerous{Label: q.Name, Keyword: got.Dangerous})
				}
				// A declared read that the classifier believes writes is worth saying
				// out loud: the declaration wins, and if it is wrong the guard that
				// would have caught it has just been bypassed.
				if q.Type == "read" && got.Op == policy.OpWrite && got.Certain {
					warnings = append(warnings, Warning{
						Code:    "DECLARED_READ_LOOKS_LIKE_WRITE",
						Message: fmt.Sprintf("query %q is declared read but its statement looks like a write", q.Name),
						Fix:     "check the statement, or change its type to write",
					})
				}
				if q.Type == "" && got.Op == policy.OpWrite {
					isWrite = true
					if !got.Certain {
						warnings = append(warnings, Warning{
							Code:    "UNRECOGNISED_STATEMENT",
							Message: fmt.Sprintf("query %q was not recognised and is being treated as a write", q.Name),
							Fix:     "declare its type explicitly with type: read or type: write",
						})
					}
				}
			}
			if isWrite {
				writes["db"] = append(writes["db"], q.Name)
			}
		}
	}

	if c.Redis != nil {
		for _, cmd := range c.Redis.Commands {
			commands := make([][]string, 0, len(cmd.Pipeline)+1)
			if len(cmd.Cmd) > 0 {
				commands = append(commands, cmd.Cmd)
			}
			commands = append(commands, cmd.Pipeline...)

			isWrite := cmd.Type == "write"
			for _, args := range commands {
				got := policy.ClassifyRedis(args)
				if got.Dangerous != "" {
					dangerous["redis"] = append(dangerous["redis"], policy.Dangerous{Label: cmd.Name, Keyword: got.Dangerous})
				}
				if got.Blocking != "" {
					warnings = append(warnings, Warning{
						Code:    "DANGEROUS_COMMAND",
						Message: fmt.Sprintf("command %q uses %s, which blocks the server for time proportional to the data size", cmd.Name, got.Blocking),
						Fix:     "it will distort both the target and the measurement; prefer SCAN, or accept that this run measures the blocking call",
					})
				}
				if cmd.Type == "" && got.Op == policy.OpWrite {
					isWrite = true
					if !got.Certain {
						warnings = append(warnings, Warning{
							Code:    "UNRECOGNISED_STATEMENT",
							Message: fmt.Sprintf("command %q was not recognised and is being treated as a write", cmd.Name),
							Fix:     "declare its type explicitly with type: read or type: write",
						})
					}
				}
			}
			if isWrite {
				writes["redis"] = append(writes["redis"], cmd.Name)
			}
		}
	}

	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Message < warnings[j].Message })
	return writes, dangerous, warnings
}

func (c *Config) checkInsecureTLS(p policy.Policy) error {
	insecure := c.HTTP != nil && c.HTTP.Transport != nil && c.HTTP.Transport.TLS != nil &&
		c.HTTP.Transport.TLS.InsecureSkipVerify
	if c.Redis != nil && c.Redis.TLS != nil && c.Redis.TLS.InsecureSkipVerify {
		insecure = true
	}
	if !insecure || p.AllowInsecureTLS {
		return nil
	}
	return errs.New(errs.CodePolicyInsecureTLS,
		"the configuration disables certificate verification, which the policy does not allow").
		WithPath("/http/transport/tls/insecure_skip_verify").
		WithHint("supply the CA bundle with tls.ca_file, or have a human grant allow_insecure_tls in the policy")
}

// Targets lists every host this configuration will contact, so preflight can resolve
// and classify them before any load is generated.
func (c *Config) Targets() []string {
	seen := map[string]bool{}
	var out []string
	add := func(host string) {
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		out = append(out, host)
	}

	if c.HTTP != nil {
		for _, r := range c.HTTP.Requests {
			add(hostOf(c.HTTP.BaseURL, r.URL))
		}
		for _, j := range c.HTTP.Journeys {
			for _, s := range j.Steps {
				add(hostOf(c.HTTP.BaseURL, s.URL))
			}
		}
	}
	if c.DB != nil {
		add(hostFromDSN(c.DB.Driver, c.DB.DSN))
	}
	if c.Redis != nil {
		for _, addr := range append([]string{c.Redis.Addr}, c.Redis.Addrs...) {
			add(hostPort(addr))
		}
	}
	sort.Strings(out)
	return out
}

func hostOf(base, ref string) string {
	full := ref
	if base != "" {
		if b, err := url.Parse(base); err == nil {
			if u, err := b.Parse(ref); err == nil {
				full = u.String()
			}
		}
	}
	u, err := url.Parse(full)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// hostFromDSN pulls the host out of a connection string without ever revealing the
// rest of it.
func hostFromDSN(driver, dsn string) string {
	switch driver {
	case "sqlite":
		return "" // a file, not a host
	case "mysql":
		// user:pass@tcp(host:port)/db
		if open := strings.Index(dsn, "("); open >= 0 {
			if shut := strings.Index(dsn[open:], ")"); shut > 0 {
				return hostPort(dsn[open+1 : open+shut])
			}
		}
		return ""
	default:
		if strings.Contains(dsn, "://") {
			if u, err := url.Parse(dsn); err == nil {
				return u.Hostname()
			}
			return ""
		}
		// libpq key=value form
		for _, field := range strings.Fields(dsn) {
			if k, v, ok := strings.Cut(field, "="); ok && strings.EqualFold(k, "host") {
				return strings.Trim(v, `'"`)
			}
		}
		return ""
	}
}

func hostPort(addr string) string {
	if addr == "" {
		return ""
	}
	if i := strings.LastIndex(addr, ":"); i > 0 && !strings.Contains(addr[i+1:], "]") {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
}

func asCoded(err error) *errs.Error {
	var typed *errs.Error
	if errors.As(err, &typed) {
		return typed
	}
	return errs.New(errs.CodePolicyDenied, "%s", err.Error())
}
