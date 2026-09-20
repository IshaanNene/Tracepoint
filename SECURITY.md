# Security

## Reporting a vulnerability

Please report security issues privately through GitHub's
[private vulnerability reporting](https://github.com/IshaanNene/Tracepoint/security/advisories/new)
rather than opening a public issue. Include the version, the configuration that
triggers it (with secrets removed) and what you observed. Expect an acknowledgement
within a few days.

## What this tool is

TracePoint generates as much load as it is told to, runs arbitrary SQL and Redis
commands, and can be driven by an autonomous agent. Every one of those is a useful
capability and a way to cause damage. The safety model exists because of that, and it
is designed for the case where **nobody is watching** — the caller is an agent, a CI
job or a scheduled run.

The full threat model is [ADR-006](docs/adr/006-policy-and-threat-model.md). In
outline:

- **Policy is granted by a human; config is written by whoever.** A configuration can
  only ever *tighten* the policy it runs under. Asking for more is refused with exit 2
  and a message naming what a human would have to change — never silently clamped,
  because a clamped run measures something other than what was asked for.
- **Targets are resolved and classified before load starts.** Loopback, private and
  link-local pass. Public addresses are refused unless allowlisted. There are no
  interactive confirmations: a prompt is not a control when stdin is a pipe.
- **Writes require `allow_writes`; destructive and administrative statements require
  `allow_dangerous` as well.** Classification fails closed.
- **The abort guard** stops a run when errors exceed 50% for 10 seconds, so a broken
  deployment is not hammered unattended.
- **Secrets are redacted at the boundary** — in logs, `result.json`, HTML, digests,
  events, errors and operation results alike. A sentinel-secret test asserts the value
  appears in no artifact.
- **Target output is untrusted input.** Anything a target said is truncated, stripped
  of control characters and labelled; response bodies never enter a digest or an
  operation result. A target cannot instruct an agent reading a TracePoint result.
- **The local MCP and REST servers** bind 127.0.0.1, require a bearer token, validate
  `Host` and `Origin`, and enable no CORS.
- **The HTML report is offline and inert**: zero network requests, a strict CSP, and
  data embedded through Go's HTML-escaping JSON encoder.

## Out of scope

A user with shell access and a legitimate policy file can do anything the tool can do.
Policy constrains agents and mistakes, not a determined operator. TracePoint is not a
multi-tenant service, and its HTTP transports are single-user loopback servers with a
bearer token rather than an authorization system.

## Supported versions

Pre-1.0: only the latest release receives fixes.
