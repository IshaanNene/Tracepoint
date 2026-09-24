package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IshaanNene/Tracepoint/internal/adapters/mcp"
	"github.com/IshaanNene/Tracepoint/internal/adapters/rest"
	"github.com/IshaanNene/Tracepoint/internal/cli"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/events"
	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/ops/opstest"
)

func capabilities(t *testing.T) cli.Capabilities {
	t.Helper()
	r := exec(t, "capabilities", "--output", "json")
	if r.code != 0 {
		t.Fatalf("capabilities: %d %s", r.code, r.stderr)
	}
	var caps cli.Capabilities
	if err := json.Unmarshal([]byte(r.stdout), &caps); err != nil {
		t.Fatalf("%v: %s", err, r.stdout)
	}
	return caps
}

// The registry, the MCP tools, the REST paths, the capabilities manifest and the CLI
// never drift from each other (§6.3).
func TestParity(t *testing.T) {
	var registry []string
	for _, op := range ops.Registry() {
		registry = append(registry, op.Name)
	}

	// MCP, over a real client session.
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := mcp.New(opstest.New(t).Service, nil).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "parity"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(ctx, nil)
	_ = cs.Close()
	_ = ss.Wait()
	if err != nil {
		t.Fatal(err)
	}
	var mcpNames []string
	for _, tool := range tools.Tools {
		mcpNames = append(mcpNames, tool.Name)
	}

	// REST, from the generated OpenAPI document.
	var restNames []string
	for path := range rest.OpenAPI()["paths"].(map[string]any) {
		restNames = append(restNames, strings.TrimPrefix(path, rest.Prefix))
	}

	caps := capabilities(t)
	var capNames []string
	for _, o := range caps.Operations {
		capNames = append(capNames, o.Name)
		if o.MCPTool != o.Name || o.RESTPath != "POST "+rest.Prefix+o.Name {
			t.Errorf("%s: mcp %q rest %q", o.Name, o.MCPTool, o.RESTPath)
		}
	}

	for name, got := range map[string][]string{"mcp": mcpNames, "rest": restNames, "capabilities": capNames} {
		slices.Sort(got)
		if !slices.Equal(got, registry) {
			t.Errorf("%s operations %v, registry %v", name, got, registry)
		}
	}

	// Every operation with a CLI equivalent names a command and flags that exist.
	commands := map[string]cli.CommandInfo{}
	for _, c := range caps.Commands {
		commands[c.Path] = c
	}
	withCLI := 0
	for _, op := range ops.Registry() {
		if op.CLI == "" {
			continue
		}
		withCLI++
		fields := strings.Fields(op.CLI)
		cmd, ok := commands[fields[0]]
		if !ok {
			t.Errorf("%s: no command %q", op.Name, fields[0])
			continue
		}
		for _, f := range fields[1:] {
			name := strings.TrimPrefix(f, "--")
			if !slices.ContainsFunc(cmd.Flags, func(fi cli.FlagInfo) bool { return fi.Name == name }) {
				t.Errorf("%s: %s has no flag %s", op.Name, fields[0], f)
			}
		}
	}
	if withCLI < 8 {
		t.Fatalf("only %d operations have a CLI equivalent", withCLI)
	}
}

// The manifest is generated from the tree, so it lists what the binary has.
func TestCapabilities(t *testing.T) {
	caps := capabilities(t)
	for _, want := range []string{"run", "validate", "doctor", "digest", "status", "wait", "stop", "list", "gc", "init", "schema", "capabilities", "mcp", "serve", "version"} {
		if !slices.ContainsFunc(caps.Commands, func(c cli.CommandInfo) bool { return c.Path == want }) {
			t.Errorf("no command %s", want)
		}
	}
	for _, c := range caps.Commands {
		if strings.HasPrefix(c.Path, "__") {
			t.Errorf("a hidden command is listed: %s", c.Path)
		}
	}
	if len(caps.ErrorCodes) != len(errs.Codes()) || len(caps.ExitCodes) < 6 {
		t.Fatalf("%d error codes, %d exit codes", len(caps.ErrorCodes), len(caps.ExitCodes))
	}
	if !slices.Contains(caps.EventTypes, events.RunFinished) || !slices.Contains(caps.Runners, "http") || !slices.Contains(caps.Drivers, "postgres") {
		t.Fatalf("registries: %v %v %v", caps.EventTypes, caps.Runners, caps.Drivers)
	}
	if caps.Contracts["result"] == "" || caps.Deprecations == nil {
		t.Fatalf("contracts %v deprecations %v", caps.Contracts, caps.Deprecations)
	}
	if !slices.ContainsFunc(caps.GlobalFlags, func(f cli.FlagInfo) bool { return f.Name == "output" }) {
		t.Fatal("no global --output")
	}
	if r := exec(t, "capabilities"); r.code != 0 || !strings.Contains(r.stdout, "wait_for_run") {
		t.Fatalf("text: %d %s", r.code, r.stdout)
	}
}

// init writes a configuration that validates, and never writes a secret into it.
func TestInit(t *testing.T) {
	out := filepath.Join(t.TempDir(), "tracepoint.yaml")
	r := exec(t, "init", "--base-url", "http://127.0.0.1:8080", "--path", "/api/items", "--db", "postgres", "--db-dsn-env", "DATABASE_URL", "--out", out)
	if r.code != 0 {
		t.Fatalf("init: %d %s", r.code, r.stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "${DATABASE_URL}") || !strings.Contains(string(b), "/api/items") {
		t.Fatalf("configuration:\n%s", b)
	}
	if r := exec(t, "init", "--base-url", "http://127.0.0.1:8080"); r.code != 0 || !strings.Contains(r.stdout, "version: 1") {
		t.Fatalf("init to stdout: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := exec(t, "init", "--base-url", "http://127.0.0.1:8080", "--db", "oracle"); r.code != errs.ExitUsage {
		t.Fatalf("an unknown driver: %d %s", r.code, r.stderr)
	}
}
