package config

import (
	"net/url"
	"regexp"
	"strings"
)

// Redacted is what replaces a secret everywhere it would otherwise appear.
const Redacted = "[redacted]"

// alwaysRedactedHeaders are redacted without being configured. Every one of them
// routinely carries a credential, and a report gets attached to tickets and pull
// requests long after anyone remembers what was in it.
//
//nolint:gochecknoglobals // A fixed table, never mutated after init.
var alwaysRedactedHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"x-auth-token":        true,
	"api-key":             true,
}

// ShouldRedactHeader reports whether a header's value must never be recorded.
func ShouldRedactHeader(name string, extra []string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if alwaysRedactedHeaders[lower] {
		return true
	}
	for _, e := range extra {
		if strings.EqualFold(strings.TrimSpace(e), lower) {
			return true
		}
	}
	return false
}

// mysqlDSN matches the user:password@ prefix of a Go MySQL DSN, which is not a URL and
// so cannot be parsed as one.
var mysqlDSN = regexp.MustCompile(`^([^:/@]+):([^@]*)@`)

// keyValueSecret matches password=... in a libpq-style connection string.
var keyValueSecret = regexp.MustCompile(`(?i)\b(password|passwd|pwd)\s*=\s*('[^']*'|"[^"]*"|[^\s;]+)`)

// RedactDSN removes the password from a connection string while leaving everything
// else legible.
//
// The host, port and database name are the useful part of a DSN in a report - they say
// what was tested - so blanking the whole string would cost real information. Three
// shapes are handled because the three drivers TracePoint supports use three:
// URL-style for Postgres, user:pass@tcp(...) for MySQL, and key=value for libpq.
func RedactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	if strings.Contains(dsn, "://") {
		if u, err := url.Parse(dsn); err == nil && u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				u.User = url.UserPassword(u.User.Username(), Redacted)
			}
			return keyValueSecret.ReplaceAllString(u.String(), "${1}="+Redacted)
		}
	}
	if m := mysqlDSN.FindStringSubmatch(dsn); len(m) > 2 && m[2] != "" {
		dsn = m[1] + ":" + Redacted + "@" + dsn[len(m[0]):]
	}
	return keyValueSecret.ReplaceAllString(dsn, "${1}="+Redacted)
}

// Redacted returns a deep copy of the configuration with every secret removed.
//
// Redaction happens here, at the boundary, rather than in each renderer. A renderer
// that forgets is a leak that nobody notices until the artifact has been shared, and
// there are five renderers; there is one of these.
func (c *Config) Redacted() *Config {
	if c == nil {
		return nil
	}
	out := c.clone()

	extra := out.Safety.AllowTargets[:0:0] // no header list in safety; kept for symmetry
	_ = extra

	if out.HTTP != nil {
		redactHeaders(out.HTTP.Headers, nil)
		for i := range out.HTTP.Requests {
			redactHeaders(out.HTTP.Requests[i].Headers, nil)
		}
		for i := range out.HTTP.Journeys {
			for j := range out.HTTP.Journeys[i].Steps {
				redactHeaders(out.HTTP.Journeys[i].Steps[j].Headers, nil)
			}
		}
	}
	if out.DB != nil {
		out.DB.DSN = RedactDSN(out.DB.DSN)
		for i := range out.DB.Queries {
			redactArgs(out.DB.Queries[i].Args)
			for j := range out.DB.Queries[i].Tx {
				redactArgs(out.DB.Queries[i].Tx[j].Args)
			}
		}
	}
	if out.Redis != nil {
		if out.Redis.Password != "" {
			out.Redis.Password = Redacted
		}
	}
	if out.Telemetry != nil {
		for _, s := range []*Sampler{out.Telemetry.Postgres, out.Telemetry.MySQL, out.Telemetry.Redis} {
			if s != nil && s.DSN != "" {
				s.DSN = RedactDSN(s.DSN)
			}
		}
	}
	return out
}

func redactHeaders(h map[string]string, extra []string) {
	for name := range h {
		if ShouldRedactHeader(name, extra) {
			h[name] = Redacted
		}
	}
}

func redactArgs(args []Arg) {
	for i := range args {
		if args[i].Secret {
			args[i].Value = Redacted
		}
	}
}

// clone makes a deep copy of everything Redacted mutates, so redacting a configuration
// never changes the one the run is using.
func (c *Config) clone() *Config {
	out := *c

	if c.HTTP != nil {
		h := *c.HTTP
		h.Headers = copyMap(c.HTTP.Headers)
		h.Requests = copySteps(c.HTTP.Requests)
		h.Journeys = make([]Journey, len(c.HTTP.Journeys))
		for i, j := range c.HTTP.Journeys {
			j.Steps = copySteps(j.Steps)
			h.Journeys[i] = j
		}
		out.HTTP = &h
	}
	if c.DB != nil {
		d := *c.DB
		d.Queries = make([]Query, len(c.DB.Queries))
		for i, q := range c.DB.Queries {
			q.Args = append([]Arg(nil), q.Args...)
			q.Tx = append([]TxStatement(nil), q.Tx...)
			for j := range q.Tx {
				q.Tx[j].Args = append([]Arg(nil), q.Tx[j].Args...)
			}
			d.Queries[i] = q
		}
		out.DB = &d
	}
	if c.Redis != nil {
		r := *c.Redis
		out.Redis = &r
	}
	if c.Telemetry != nil {
		tel := *c.Telemetry
		tel.Postgres = copySampler(c.Telemetry.Postgres)
		tel.MySQL = copySampler(c.Telemetry.MySQL)
		tel.Redis = copySampler(c.Telemetry.Redis)
		out.Telemetry = &tel
	}
	return &out
}

func copySampler(s *Sampler) *Sampler {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

func copySteps(in []HTTPStep) []HTTPStep {
	out := make([]HTTPStep, len(in))
	for i, s := range in {
		s.Headers = copyMap(s.Headers)
		out[i] = s
	}
	return out
}

func copyMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
