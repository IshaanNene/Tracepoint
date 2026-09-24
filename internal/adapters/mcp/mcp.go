// Package mcp exposes the operation registry as a Model Context Protocol server, using
// the official Go SDK. It holds no logic of its own: every tool is generated from
// internal/ops, and a parity test holds the two together (ADR-008).
//
// It builds only on tools, resources and prompts - not roots, sampling or logging,
// which the current specification revision deprecates.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IshaanNene/Tracepoint/docs"
	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
)

// instructions is what a client shows the model about this server as a whole.
const instructions = `TracePoint drives HTTP, SQL and Redis load on one clock and reports which tier a slowdown came from, with evidence and a confidence level.
Workflow: get_policy; scaffold_config or write a configuration; validate_config; plan_run; start_run a 30s smoke run; wait_for_run until finished; read get_run_digest - validity first, because an invalid run measured the generator, not the target. Follow a digest's rerun recommendations with start_run and its overrides, one change at a time.
Never target production without explicit allowlisting by a human, never loosen the policy, run one test against a target at a time, and report correlation as association, not proof.`

// New builds the server over a service.
func New(svc *ops.Service, log *slog.Logger) *sdk.Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	bi := buildinfo.Get()
	s := sdk.NewServer(&sdk.Implementation{Name: "tracepoint", Title: "TracePoint", Version: bi.Version},
		&sdk.ServerOptions{Instructions: instructions, Logger: log})

	for _, op := range ops.Registry() {
		s.AddTool(Tool(op), handler(op, svc))
	}
	addResources(s, svc)
	addPrompts(s)
	return s
}

// Tool is the MCP definition of an operation.
func Tool(op ops.Operation) *sdk.Tool {
	openWorld := op.Annotations.OpenWorld
	destructive := op.Annotations.Destructive
	return &sdk.Tool{
		Name: op.Name, Title: op.Title, Description: op.Description,
		InputSchema: op.Input, OutputSchema: op.Output,
		Annotations: &sdk.ToolAnnotations{
			Title: op.Title, ReadOnlyHint: op.Annotations.ReadOnly, IdempotentHint: op.Annotations.Idempotent,
			DestructiveHint: &destructive, OpenWorldHint: &openWorld,
		},
	}
}

// handler runs one operation. Its result is structured content plus a one-line text
// summary, for clients that do not render structured output. A failure is a tool
// result with isError set - the model has to see it to correct course - carrying the
// coded error envelope.
func handler(op ops.Operation, svc *ops.Service) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		call := *svc
		call.Actor = actor(req.Session)
		out, err := op.Invoke(ctx, &call, req.Params.Arguments)
		text := ops.Summary(op, out, err)
		if err != nil {
			env := errs.EnvelopeOf(err)
			body, merr := json.Marshal(env)
			if merr != nil {
				body = []byte(`{"error":{"code":"INTERNAL"}}`)
			}
			return &sdk.CallToolResult{
				IsError: true,
				Content: []sdk.Content{&sdk.TextContent{Text: text}, &sdk.TextContent{Text: string(body)}},
			}, nil
		}
		return &sdk.CallToolResult{
			Content:           []sdk.Content{&sdk.TextContent{Text: text}},
			StructuredContent: out,
		}, nil
	}
}

// actor names the client in the audit log, from what it said at initialisation.
func actor(ss *sdk.ServerSession) string {
	if ss != nil {
		if p := ss.InitializeParams(); p != nil && p.ClientInfo != nil && p.ClientInfo.Name != "" {
			return "mcp:" + errs.CleanUntrusted(p.ClientInfo.Name)
		}
	}
	return "mcp"
}

// Resource URIs.
const (
	schemeSchemas = "tracepoint://schemas/"
	schemeDocs    = "tracepoint://docs/"
	runDigest     = "tracepoint://runs/{run_id}/digest"
	runResult     = "tracepoint://runs/{run_id}/result"
)

func addResources(s *sdk.Server, svc *ops.Service) {
	for _, name := range schemas.Names() {
		uri := schemeSchemas + string(name)
		s.AddResource(&sdk.Resource{
			URI: uri, Name: string(name) + "-schema", MIMEType: "application/schema+json",
			Description: "JSON Schema for " + schemas.Describe(name) + ", with a description and example on every field.",
		}, func(_ context.Context, _ *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			b, err := schemas.Get(name)
			if err != nil {
				return nil, err
			}
			return textResource(uri, "application/schema+json", string(b)), nil
		})
	}
	for name, doc := range map[string]struct{ desc, body string }{
		"methodology": {"How a verdict is reached: hot buckets, incidents, culprits, correlation, confidence and validity, with every constant.", docs.Methodology},
		"errors":      {"Every error and finding code, its exit code and its fix.", docs.Errors},
	} {
		uri := schemeDocs + name
		body := doc.body
		s.AddResource(&sdk.Resource{URI: uri, Name: name, MIMEType: "text/markdown", Description: doc.desc},
			func(_ context.Context, _ *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
				return textResource(uri, "text/markdown", body), nil
			})
	}
	s.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: runDigest, Name: "run-digest", MIMEType: "application/json",
		Description: "A finished run's digest: the prioritised summary to read first.",
	}, runFile(svc, "digest"))
	s.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: runResult, Name: "run-result", MIMEType: "application/json",
		Description: "A finished run's full result.json. Large; prefer the digest and get_run_section.",
	}, runFile(svc, "result"))
}

func runFile(svc *ops.Service, kind string) sdk.ResourceHandler {
	return func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		uri := req.Params.URI
		rest, ok := strings.CutPrefix(uri, "tracepoint://runs/")
		id, file, found := strings.Cut(rest, "/")
		if !ok || !found || file != kind || id == "" {
			return nil, sdk.ResourceNotFoundError(uri)
		}
		r, err := svc.Store.Get(id)
		if err != nil {
			return nil, sdk.ResourceNotFoundError(uri)
		}
		name := runstore.ResultFile
		if kind == "digest" {
			name = runstore.DigestFile
		}
		b, err := os.ReadFile(filepath.Clean(r.Path(name)))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, sdk.ResourceNotFoundError(uri)
			}
			return nil, fmt.Errorf("reading %s: %w", uri, err)
		}
		return textResource(uri, "application/json", string(b)), nil
	}
}

func textResource(uri, mime, text string) *sdk.ReadResourceResult {
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: mime, Text: text}}}
}

// prompts are the workflows the bundled skill teaches, offered to clients that surface
// prompts to their users.
var prompts = []struct {
	name, title, desc string
	args              []*sdk.PromptArgument
	body              func(map[string]string) string
}{
	{
		name: "hunt_bottleneck", title: "Find which tier is slow",
		desc: "Run a correlated load test and report which tier - application, database or cache - a slowdown comes from.",
		args: []*sdk.PromptArgument{
			{Name: "target", Description: "The application's base URL, or a configuration path.", Required: true},
			{Name: "slo", Description: "The latency budget, such as p99 250ms."},
		},
		body: func(a map[string]string) string {
			return fmt.Sprintf(`Find which tier makes %s slow under load.
1. Call get_policy. Ask me which targets and environments you may load, whether writes are allowed, the SLOs%s and the time budget. Do not assume, and never target production unless I have allowlisted it.
2. Write a configuration with scaffold_config (probe the database and cache it uses), then validate_config and plan_run, and show me the plan.
3. start_run a 30-second smoke run; wait_for_run until finished. Its digest must be valid before anything else is read.
4. Run the baseline, read get_run_digest - validity, then the verdict with its evidence and confidence - and use get_run_section only for what the digest does not answer.
5. Test the verdict by following its rerun recommendations one at a time.
6. Report the verdict, its confidence, the evidence, the caveats and the run directory. It is correlation on a shared clock, not proof; say so.`, a["target"], orNothing(" (I suggested: ", a["slo"], ")"))
		},
	},
	{
		name: "capacity_check", title: "Check how much load it takes",
		desc: "Find the load at which latency starts to degrade and which tier gives out first.",
		args: []*sdk.PromptArgument{{Name: "target", Description: "The application's base URL, or a configuration path.", Required: true}},
		body: func(a map[string]string) string {
			return fmt.Sprintf(`Find how much load %s takes before latency degrades, and which tier gives out first.
Confirm the policy and the allowed targets with me first. Start from a valid smoke run, then raise the rate in steps with start_run and its overrides, waiting for each run and reading its digest. Stop raising at the first invalid or SLO-breaching run and report the last level that held, the first that broke, and the verdict at the break. Never raise the rate past the policy, and never loosen it.`, a["target"])
		},
	},
	{
		name: "regression_gate", title: "Check a change for a latency regression",
		desc: "Run the same test before and after a change and say whether latency regressed.",
		args: []*sdk.PromptArgument{
			{Name: "baseline_run", Description: "The run id to compare against.", Required: true},
		},
		body: func(a map[string]string) string {
			return fmt.Sprintf(`Check whether a change regressed latency against run %s.
Re-run its configuration with start_run (same seed), wait_for_run, and compare both digests: validity first, then p99 and error ratio per runner, then incidents. Treat a difference within noise as no regression, and say which tier moved if one did.`, a["baseline_run"])
		},
	},
}

func addPrompts(s *sdk.Server) {
	for _, p := range prompts {
		p := p
		s.AddPrompt(&sdk.Prompt{Name: p.name, Title: p.title, Description: p.desc, Arguments: p.args},
			func(_ context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
				args := req.Params.Arguments
				for _, a := range p.args {
					if a.Required && args[a.Name] == "" {
						return nil, fmt.Errorf("argument %q is required", a.Name)
					}
				}
				return &sdk.GetPromptResult{Description: p.desc, Messages: []*sdk.PromptMessage{
					{Role: "user", Content: &sdk.TextContent{Text: p.body(args)}},
				}}, nil
			})
	}
}

func orNothing(prefix, v, suffix string) string {
	if v == "" {
		return ""
	}
	return prefix + v + suffix
}
