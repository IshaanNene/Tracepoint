// Package errs defines TracePoint's coded errors and the JSON envelope they are
// reported in.
//
// Every failure carries a stable Code. Callers - including agents - branch on the
// code, never on the message, because messages are written for humans and may be
// reworded in any release while renaming a code is a breaking change (spec §6.7).
// The registry lives in docs/ERRORS.md and a test asserts the two cannot drift.
package errs

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error identifier.
type Code string

// Exit codes, part of the CLI contract (spec §6.1).
const (
	ExitOK       = 0 // success
	ExitBreach   = 1 // SLO breach or regression
	ExitUsage    = 2 // usage, config or policy refusal
	ExitRuntime  = 3 // preflight or runtime failure, or aborted
	ExitInvalid  = 4 // run invalid (unless --allow-invalid)
	ExitWaitOpen = 5 // wait timed out while the run continues
)

// Configuration codes. All exit 2 and none are retriable: running the same input
// again cannot produce a different answer.
const (
	CodeConfigNotFound           Code = "CONFIG_NOT_FOUND"
	CodeConfigParse              Code = "CONFIG_PARSE"
	CodeConfigUnknownField       Code = "CONFIG_UNKNOWN_FIELD"
	CodeConfigMissingField       Code = "CONFIG_MISSING_FIELD"
	CodeConfigInvalidValue       Code = "CONFIG_INVALID_VALUE"
	CodeConfigVersionUnsupported Code = "CONFIG_VERSION_UNSUPPORTED"
	CodeConfigNoRunner           Code = "CONFIG_NO_RUNNER"
	CodeConfigRequestsAndJourney Code = "CONFIG_REQUESTS_AND_JOURNEYS"
	CodeConfigDuplicateName      Code = "CONFIG_DUPLICATE_NAME"
	CodeConfigEnvUnset           Code = "CONFIG_ENV_UNSET"
	CodeConfigStagesMismatch     Code = "CONFIG_STAGES_MISMATCH"
	CodeConfigTemplateSyntax     Code = "CONFIG_TEMPLATE_SYNTAX"
	CodeConfigTemplateUnknownFn  Code = "CONFIG_TEMPLATE_UNKNOWN_FUNC"
	CodeConfigUndefinedVariable  Code = "CONFIG_UNDEFINED_VARIABLE"
	CodeConfigFeederNotFound     Code = "CONFIG_FEEDER_NOT_FOUND"
	CodeSetParse                 Code = "SET_PARSE"
	CodeSetUnknownPath           Code = "SET_UNKNOWN_PATH"
	CodeSetTypeMismatch          Code = "SET_TYPE_MISMATCH"
	CodeSetIndexNotFound         Code = "SET_INDEX_NOT_FOUND"
)

// Policy codes. All exit 2. Each names what a human would have to change; an agent's
// correct response is to report the refusal, not to work around it.
const (
	CodePolicyDenied              Code = "POLICY_DENIED"
	CodePolicyParse               Code = "POLICY_PARSE"
	CodePolicyTargetNotAllowed    Code = "POLICY_TARGET_NOT_ALLOWED"
	CodePolicyWritesNotAllowed    Code = "POLICY_WRITES_NOT_ALLOWED"
	CodePolicyDangerousNotAllowed Code = "POLICY_DANGEROUS_NOT_ALLOWED"
	CodePolicyRateExceeded        Code = "POLICY_RATE_EXCEEDED"
	CodePolicyDurationExceeded    Code = "POLICY_DURATION_EXCEEDED"
	CodePolicyInsecureTLS         Code = "POLICY_INSECURE_TLS_NOT_ALLOWED"
	CodePolicyTooManyRuns         Code = "POLICY_TOO_MANY_RUNS"
	CodePolicyDetachNotAllowed    Code = "POLICY_DETACH_NOT_ALLOWED"
)

// Preflight codes. All exit 3. Preflight runs before any load, so these cost only time.
const (
	CodePreflightDNS           Code = "PREFLIGHT_DNS"
	CodePreflightConnect       Code = "PREFLIGHT_CONNECT"
	CodePreflightHTTP          Code = "PREFLIGHT_HTTP"
	CodePreflightDBPing        Code = "PREFLIGHT_DB_PING"
	CodePreflightDBPrepare     Code = "PREFLIGHT_DB_PREPARE"
	CodePreflightRedisPing     Code = "PREFLIGHT_REDIS_PING"
	CodePreflightDriverUnknown Code = "PREFLIGHT_DRIVER_UNKNOWN"
)

// Run outcome codes.
const (
	CodeRunAborted     Code = "RUN_ABORTED"     // exit 3, abort guard tripped
	CodeRunInterrupted Code = "RUN_INTERRUPTED" // exit 3, signal or stop
	CodeRunFailed      Code = "RUN_FAILED"      // exit 3
	CodeRunInvalid     Code = "RUN_INVALID"     // exit 4, the generator was the bottleneck
	CodeSLOBreach      Code = "SLO_BREACH"      // exit 1, a successful measurement of a failing target
)

// Result, comparison and run-store codes.
const (
	CodeResultNotFound          Code = "RESULT_NOT_FOUND"
	CodeResultParse             Code = "RESULT_PARSE"
	CodeResultSchemaUnsupported Code = "RESULT_SCHEMA_UNSUPPORTED"
	CodeCompareSchemaMismatch   Code = "COMPARE_SCHEMA_MISMATCH"
	CodeCompareInsufficient     Code = "COMPARE_INSUFFICIENT_DATA"
	CodeCompareRegression       Code = "COMPARE_REGRESSION"
	CodeRunNotFound             Code = "RUN_NOT_FOUND"
	CodeRunAlreadyRunning       Code = "RUN_ALREADY_RUNNING"
	CodeRunLost                 Code = "RUN_LOST"
	CodeWaitTimeout             Code = "WAIT_TIMEOUT"
)

// Server, operation and I/O codes.
const (
	CodeOpsUnknownOperation Code = "OPS_UNKNOWN_OPERATION"
	CodeOpsInvalidInput     Code = "OPS_INVALID_INPUT"
	CodeServerUnauthorized  Code = "SERVER_UNAUTHORIZED"
	CodeServerBadHost       Code = "SERVER_BAD_HOST"
	CodeServerBadOrigin     Code = "SERVER_BAD_ORIGIN"
	CodeServerAddrInUse     Code = "SERVER_ADDR_IN_USE"
	CodeIOWriteFailed       Code = "IO_WRITE_FAILED"
	CodeIOReadFailed        Code = "IO_READ_FAILED"
	CodeInternal            Code = "INTERNAL"
)

// meta describes a code's fixed properties. Keeping them in one table is what lets
// the CLI, the servers and the capabilities manifest agree without repeating
// themselves, and what lets a test check the table against docs/ERRORS.md.
type meta struct {
	exit      int
	retriable bool
	docs      string
}

//nolint:gochecknoglobals // A fixed lookup table, never mutated after init.
var registry = map[Code]meta{
	CodeConfigNotFound:           {ExitUsage, false, "docs/ERRORS.md#configuration--exit-2-never-retriable"},
	CodeConfigParse:              {ExitUsage, false, "docs/ERRORS.md#configuration--exit-2-never-retriable"},
	CodeConfigUnknownField:       {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigMissingField:       {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigInvalidValue:       {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigVersionUnsupported: {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigNoRunner:           {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigRequestsAndJourney: {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigDuplicateName:      {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigEnvUnset:           {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigStagesMismatch:     {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigTemplateSyntax:     {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigTemplateUnknownFn:  {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigUndefinedVariable:  {ExitUsage, false, "docs/CONFIG.md"},
	CodeConfigFeederNotFound:     {ExitUsage, false, "docs/CONFIG.md"},
	CodeSetParse:                 {ExitUsage, false, "docs/CLI.md"},
	CodeSetUnknownPath:           {ExitUsage, false, "docs/CLI.md"},
	CodeSetTypeMismatch:          {ExitUsage, false, "docs/CLI.md"},
	CodeSetIndexNotFound:         {ExitUsage, false, "docs/CLI.md"},

	CodePolicyDenied:              {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyParse:               {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyTargetNotAllowed:    {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyWritesNotAllowed:    {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyDangerousNotAllowed: {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyRateExceeded:        {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyDurationExceeded:    {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyInsecureTLS:         {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyTooManyRuns:         {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},
	CodePolicyDetachNotAllowed:    {ExitUsage, false, "docs/ERRORS.md#policy--exit-2-never-retriable"},

	CodePreflightDNS:           {ExitRuntime, true, "docs/ERRORS.md#preflight--exit-3"},
	CodePreflightConnect:       {ExitRuntime, true, "docs/ERRORS.md#preflight--exit-3"},
	CodePreflightHTTP:          {ExitRuntime, true, "docs/ERRORS.md#preflight--exit-3"},
	CodePreflightDBPing:        {ExitRuntime, true, "docs/ERRORS.md#preflight--exit-3"},
	CodePreflightDBPrepare:     {ExitRuntime, false, "docs/ERRORS.md#preflight--exit-3"},
	CodePreflightRedisPing:     {ExitRuntime, true, "docs/ERRORS.md#preflight--exit-3"},
	CodePreflightDriverUnknown: {ExitUsage, false, "docs/ERRORS.md#preflight--exit-3"},

	CodeRunAborted:     {ExitRuntime, false, "docs/ERRORS.md#run--exit-3-or-4-when-invalid"},
	CodeRunInterrupted: {ExitRuntime, false, "docs/ERRORS.md#run--exit-3-or-4-when-invalid"},
	CodeRunFailed:      {ExitRuntime, false, "docs/ERRORS.md#run--exit-3-or-4-when-invalid"},
	CodeRunInvalid:     {ExitInvalid, false, "docs/METHODOLOGY.md"},
	CodeSLOBreach:      {ExitBreach, false, "docs/ERRORS.md#run--exit-3-or-4-when-invalid"},

	CodeResultNotFound:          {ExitUsage, false, "docs/ERRORS.md#results-comparison-and-the-run-store"},
	CodeResultParse:             {ExitUsage, false, "docs/ERRORS.md#results-comparison-and-the-run-store"},
	CodeResultSchemaUnsupported: {ExitRuntime, false, "docs/adr/004-result-system-of-record.md"},
	CodeCompareSchemaMismatch:   {ExitRuntime, false, "docs/adr/004-result-system-of-record.md"},
	CodeCompareInsufficient:     {ExitBreach, false, "docs/ERRORS.md#results-comparison-and-the-run-store"},
	CodeCompareRegression:       {ExitBreach, false, "docs/ERRORS.md#results-comparison-and-the-run-store"},
	CodeRunNotFound:             {ExitUsage, false, "docs/ERRORS.md#results-comparison-and-the-run-store"},
	CodeRunAlreadyRunning:       {ExitUsage, false, "docs/ERRORS.md#results-comparison-and-the-run-store"},
	CodeRunLost:                 {ExitRuntime, false, "docs/ERRORS.md#results-comparison-and-the-run-store"},
	CodeWaitTimeout:             {ExitWaitOpen, true, "docs/ERRORS.md#results-comparison-and-the-run-store"},

	CodeOpsUnknownOperation: {ExitUsage, false, "docs/ERRORS.md#servers-and-operations"},
	CodeOpsInvalidInput:     {ExitUsage, false, "docs/ERRORS.md#servers-and-operations"},
	CodeServerUnauthorized:  {ExitUsage, false, "docs/ERRORS.md#servers-and-operations"},
	CodeServerBadHost:       {ExitUsage, false, "docs/ERRORS.md#servers-and-operations"},
	CodeServerBadOrigin:     {ExitUsage, false, "docs/ERRORS.md#servers-and-operations"},
	CodeServerAddrInUse:     {ExitRuntime, true, "docs/ERRORS.md#servers-and-operations"},
	CodeIOWriteFailed:       {ExitRuntime, true, "docs/ERRORS.md#servers-and-operations"},
	CodeIOReadFailed:        {ExitRuntime, true, "docs/ERRORS.md#servers-and-operations"},
	CodeInternal:            {ExitRuntime, false, "docs/ERRORS.md#servers-and-operations"},
}

// Codes returns every registered code, sorted, for the capabilities manifest and for
// the test that checks this table against docs/ERRORS.md.
func Codes() []Code {
	out := make([]Code, 0, len(registry))
	for c := range registry {
		out = append(out, c)
	}
	sortCodes(out)
	return out
}

// Error is a TracePoint error: a stable code, a human sentence, and wherever possible
// the location and the fix.
type Error struct {
	Code    Code
	Message string
	// Path is a JSON Pointer into the offending document, when there is one.
	Path         string
	Line, Column int
	// Hint says what to do about it, concretely.
	Hint    string
	Docs    string
	RunID   string
	Details map[string]any
	// Causes carries sibling problems found in the same pass, so a config with five
	// mistakes reports five rather than one at a time.
	Causes []*Error

	wrapped error
}

// New builds an error with a code and a formatted message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Docs: registry[code].docs}
}

// Wrap builds an error that carries an underlying cause for errors.Is and errors.As.
func Wrap(code Code, err error, format string, args ...any) *Error {
	e := New(code, format, args...)
	e.wrapped = err
	return e
}

// WithHint attaches the concrete fix.
func (e *Error) WithHint(format string, args ...any) *Error {
	e.Hint = fmt.Sprintf(format, args...)
	return e
}

// WithPath attaches a JSON Pointer to the offending field.
func (e *Error) WithPath(path string) *Error { e.Path = path; return e }

// WithPos attaches a 1-based source position.
func (e *Error) WithPos(line, column int) *Error { e.Line, e.Column = line, column; return e }

// WithRunID attaches the run this error belongs to.
func (e *Error) WithRunID(id string) *Error { e.RunID = id; return e }

// WithDetail attaches one structured detail. Details must never carry secrets or
// response bodies (ADR-006).
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// WithCauses attaches sibling problems found in the same pass.
func (e *Error) WithCauses(causes ...*Error) *Error {
	e.Causes = append(e.Causes, causes...)
	return e
}

func (e *Error) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.wrapped)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.wrapped }

// ExitCode is the process exit code for this error.
func (e *Error) ExitCode() int {
	if m, ok := registry[e.Code]; ok {
		return m.exit
	}
	return ExitRuntime
}

// Retriable reports whether repeating the identical call could plausibly succeed.
func (e *Error) Retriable() bool { return registry[e.Code].retriable }

// ExitCodeOf finds the exit code for any error, defaulting to ExitRuntime for errors
// that did not come from this package.
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return ExitRuntime
}

// precedence orders exit codes for the case where several apply at once. Spec §6.1
// fixes this as 2 > 3 > 4 > 1: a refused config never reports a verdict it did not
// produce, and an invalid run never reports an SLO result that cannot be believed.
//
//nolint:gochecknoglobals // A fixed lookup table, never mutated after init.
var precedence = map[int]int{ExitUsage: 4, ExitRuntime: 3, ExitInvalid: 2, ExitBreach: 1, ExitWaitOpen: 1, ExitOK: 0}

// Worst returns the exit code that wins when several apply.
func Worst(codes ...int) int {
	worst := ExitOK
	for _, c := range codes {
		if precedence[c] > precedence[worst] {
			worst = c
		}
	}
	return worst
}

// doc is the wire shape of an error, matching schemas/error.schema.json exactly.
type doc struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	Path      string         `json:"path,omitempty"`
	Line      int            `json:"line,omitempty"`
	Column    int            `json:"column,omitempty"`
	Hint      string         `json:"hint,omitempty"`
	Docs      string         `json:"docs,omitempty"`
	ExitCode  int            `json:"exit_code,omitempty"`
	Retriable bool           `json:"retriable"`
	RunID     string         `json:"run_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	Causes    []nestedDoc    `json:"causes,omitempty"`
}

type nestedDoc struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Docs    string `json:"docs,omitempty"`
}

// Envelope is the single JSON document written to stdout when a command run with
// --output json fails (spec §6.2).
type Envelope struct {
	Error doc `json:"error"`
}

// MarshalJSON renders the error as its envelope, so that json.Marshal on an *Error
// cannot accidentally produce a shape the schema does not describe.
func (e *Error) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(e.Envelope())
	if err != nil {
		return nil, fmt.Errorf("marshalling error envelope: %w", err)
	}
	return b, nil
}

// Envelope converts the error into its wire form.
func (e *Error) Envelope() Envelope {
	d := doc{
		Code:      e.Code,
		Message:   e.Message,
		Path:      e.Path,
		Line:      e.Line,
		Column:    e.Column,
		Hint:      e.Hint,
		Docs:      e.Docs,
		ExitCode:  e.ExitCode(),
		Retriable: e.Retriable(),
		RunID:     e.RunID,
		Details:   e.Details,
	}
	for _, c := range e.Causes {
		d.Causes = append(d.Causes, nestedDoc{
			Code: c.Code, Message: c.Message, Path: c.Path,
			Line: c.Line, Column: c.Column, Hint: c.Hint, Docs: c.Docs,
		})
	}
	return Envelope{Error: d}
}

// EnvelopeOf converts any error into an envelope, so a stray non-TracePoint error
// still leaves stdout holding exactly one valid JSON document.
func EnvelopeOf(err error) Envelope {
	var e *Error
	if errors.As(err, &e) {
		return e.Envelope()
	}
	return New(CodeInternal, "%s", err.Error()).
		WithHint("this is a bug; please report it with the command you ran").
		Envelope()
}

func sortCodes(s []Code) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
