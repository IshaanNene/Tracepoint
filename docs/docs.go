// Package docs embeds the documents an agent may need at run time, so that the MCP
// server serves exactly the methodology and error registry of the binary it runs in.
package docs

import _ "embed"

// Methodology is METHODOLOGY.md: how a verdict is reached, with every constant.
//
//go:embed METHODOLOGY.md
var Methodology string

// Errors is ERRORS.md: every error and finding code, its exit code and its fix.
//
//go:embed ERRORS.md
var Errors string
