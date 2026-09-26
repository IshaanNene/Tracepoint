package config

import "regexp"

// CodeProbeTrivial marks a database probe that cannot see the tables the application
// uses. It is info, not a warning: the probe still measures something real.
const CodeProbeTrivial = "PROBE_TRIVIAL"

// trivialSQL matches a query that touches no table: SELECT 1, SELECT 1 FROM DUAL.
var trivialSQL = regexp.MustCompile(`(?i)^\s*select\s+1(\s+from\s+dual)?\s*;?\s*$`)

// Caveats are what a configuration cannot see, stated so that a clean result is not
// read as more than it proves (§7: the quick probe's blind spots belong in the
// digest).
func (c *Config) Caveats() []Warning {
	var out []Warning
	if d := c.DB; d != nil && len(d.Queries) > 0 {
		trivial := true
		for _, q := range d.Queries {
			if len(q.Tx) > 0 || !trivialSQL.MatchString(q.SQL) {
				trivial = false
				break
			}
		}
		if trivial {
			out = append(out, Warning{
				Code: CodeProbeTrivial, Severity: "info",
				Message: "the db probe runs SELECT 1, which sees the connection, the network and the server's scheduler but none of the application's tables: a lock, a slow plan or a hot row there will not show in the probe, so a healthy probe does not clear the database",
				Fix:     "probe with a query the application runs, such as a primary-key lookup on its busiest table",
			})
		}
	}
	return out
}
