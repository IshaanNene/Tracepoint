package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/IshaanNene/Tracepoint/internal/adapters/guard"
	tpmcp "github.com/IshaanNene/Tracepoint/internal/adapters/mcp"
	"github.com/IshaanNene/Tracepoint/internal/adapters/rest"
	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/events"
	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/result"
	"github.com/IshaanNene/Tracepoint/internal/runner"
	"github.com/IshaanNene/Tracepoint/internal/runner/sqlrun"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/session"
	"github.com/IshaanNene/Tracepoint/internal/telemetry"
)

// serverFlags are shared by mcp and serve.
type serverFlags struct {
	policyPath  string
	runRoot     string
	token       string
	tokenFile   string
	origins     []string
	hosts       []string
	allowRemote bool
}

func (s *serverFlags) register(f *pflag.FlagSet) {
	f.StringVar(&s.policyPath, "policy", "", "safety policy file, fixed for the server's lifetime; defaults to $TRACEPOINT_POLICY, then to the server envelope (private targets only, no writes, 500 req/s, 10m, one run at a time)")
	f.StringVar(&s.runRoot, "run-root", "", "directory runs are created under; defaults to the policy's run_root")
	f.StringVar(&s.token, "token", "", "bearer token for HTTP transports; defaults to $TRACEPOINT_TOKEN, then to one generated into a 0600 file")
	f.StringVar(&s.tokenFile, "token-file", "", "where a generated token is kept; defaults to <run-root>/.<command>-token")
	f.StringArrayVar(&s.origins, "allow-origin", nil, "an Origin header to accept; by default any request carrying one is refused")
	f.StringArrayVar(&s.hosts, "allow-host", nil, "an extra Host header to accept, as host:port")
	f.BoolVar(&s.allowRemote, "allow-remote", false, "allow listening on a non-loopback address")
}

// service builds the operation service with the policy fixed at startup.
func (s *serverFlags) service(env Env, g *globals) (*ops.Service, error) {
	granted := policy.ServerDefault()
	if s.policyPath != "" {
		p, err := policy.Load(s.policyPath, env.Lookup)
		if err != nil {
			return nil, err
		}
		granted = p
	} else if v, ok := env.Lookup(policy.EnvVar); ok && v != "" {
		p, err := policy.Load(v, env.Lookup)
		if err != nil {
			return nil, err
		}
		granted = p
	}
	store, err := openStore(env, granted, s.runRoot)
	if err != nil {
		return nil, err
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "finding the working directory")
	}
	exe := env.Executable
	return &ops.Service{
		Policy: granted, Store: store, Lookup: env.Lookup, Root: root, Logger: g.logger(env),
		Launch: func(ctx context.Context, p *session.Prepared) (int, error) { return p.Detach(ctx, exe) },
	}, nil
}

// guardFor builds the HTTP guard for a listen address.
func (s *serverFlags) guardFor(env Env, addr, name string, store *runstore.Store) (*guard.Guard, string, error) {
	if err := guard.CheckBind(addr, s.allowRemote); err != nil {
		return nil, "", err
	}
	token := s.token
	if token == "" {
		if v, ok := env.Lookup("TRACEPOINT_TOKEN"); ok {
			token = v
		}
	}
	file := s.tokenFile
	if file == "" {
		file = filepath.Join(store.Root(), "."+name+"-token")
	}
	token, path, err := guard.Token(token, file)
	if err != nil {
		return nil, "", err
	}
	g, err := guard.New(addr, guard.Options{Token: token, Hosts: s.hosts, Origins: s.origins, Open: []string{rest.HealthPath}})
	return g, path, err
}

func newMCPCmd(env Env, g *globals) *cobra.Command {
	var (
		sf   serverFlags
		addr string
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the agent operations over the Model Context Protocol",
		Long: strings.TrimSpace(`
Run an MCP server exposing every agent operation as a tool, the JSON Schemas and run
digests as resources, and the hunt_bottleneck, capacity_check and regression_gate
prompts. It speaks stdio by default, which is what a client that launches it expects;
--http serves streamable HTTP instead, on loopback, behind a bearer token and Host and
Origin checks.

The safety policy is fixed when the server starts. No tool call can exceed it; a call
that tries is refused with a POLICY_* code naming what a human would have to change.`),
		Example: strings.TrimSpace(`
  claude mcp add --transport stdio tracepoint -- tracepoint mcp
  tracepoint mcp --policy policy.yaml
  tracepoint mcp --http 127.0.0.1:7475`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := sf.service(env, g)
			if err != nil {
				return err
			}
			server := tpmcp.New(svc, g.logger(env))
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if addr == "" {
				// stdout belongs to the protocol; every log line goes to stderr.
				t := &sdk.IOTransport{Reader: io.NopCloser(stdinOf(env)), Writer: nopWriteCloser{env.Stdout}}
				if rerr := server.Run(ctx, t); rerr != nil && !errors.Is(rerr, context.Canceled) && !errors.Is(rerr, io.EOF) {
					return errs.Wrap(errs.CodeInternal, rerr, "serving MCP over stdio")
				}
				return nil
			}
			gd, tokenPath, err := sf.guardFor(env, addr, "mcp", svc.Store)
			if err != nil {
				return err
			}
			h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
			return serveHTTP(ctx, env, g, addr, gd.Wrap(h), "mcp", tokenPath)
		},
	}
	sf.register(cmd.Flags())
	cmd.Flags().StringVar(&addr, "http", "", "serve streamable HTTP on this address, such as 127.0.0.1:7475, instead of stdio")
	return cmd
}

func newServeCmd(env Env, g *globals) *cobra.Command {
	var (
		sf   serverFlags
		addr string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the agent operations as a REST API with an OpenAPI document",
		Long: strings.TrimSpace(`
Serve every agent operation as POST /v1/ops/<name>, with the OpenAPI 3.1 document at
/v1/openapi.json, for agent frameworks and services that do not speak MCP. It listens
on loopback behind a bearer token, validates the Host and Origin headers and sends no
CORS headers. The safety policy is fixed at startup.`),
		Example: strings.TrimSpace(`
  tracepoint serve
  tracepoint serve --listen 127.0.0.1:7474 --policy policy.yaml
  curl -H "Authorization: Bearer $(cat runs/.serve-token)" -d '{}' localhost:7474/v1/ops/get_policy`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := sf.service(env, g)
			if err != nil {
				return err
			}
			gd, tokenPath, err := sf.guardFor(env, addr, "serve", svc.Store)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serveHTTP(ctx, env, g, addr, gd.Wrap(rest.Handler(svc)), "serve", tokenPath)
		},
	}
	sf.register(cmd.Flags())
	cmd.Flags().StringVar(&addr, "listen", "127.0.0.1:7474", "address to listen on; loopback unless --allow-remote")
	return cmd
}

// serveHTTP runs a server until ctx ends, then shuts it down gracefully.
func serveHTTP(ctx context.Context, env Env, g *globals, addr string, h http.Handler, name, tokenPath string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return errs.Wrap(errs.CodeServerAddrInUse, err, "listening on %s", addr).
			WithHint("choose another address with --listen or --http")
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	log := g.logger(env)
	log.Info("listening", "server", name, "addr", ln.Addr().String(), "token_file", tokenPath)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return errs.Wrap(errs.CodeInternal, err, "serving %s", name)
		}
		return nil
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "shutting down %s", name)
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func stdinOf(env Env) io.Reader {
	if env.Stdin != nil {
		return env.Stdin
	}
	return os.Stdin
}

func newInitCmd(env Env, g *globals) *cobra.Command {
	var (
		in  ops.ScaffoldIn
		out string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a commented starter configuration",
		Long: strings.TrimSpace(`
Write a commented starter configuration for an application and, optionally, the database
and Redis it uses, each probed alongside it. Connection strings are written as
environment references, never values, so the file can be committed. It contacts nothing.

--detect DIR infers what it can from a project: compose files for the database, the
cache and the application's port; environment files for variable names, never values;
and an OpenAPI document for the requests. Each inference is printed with its evidence
and a confidence. --from-openapi FILE imports requests from an OpenAPI 3 or Swagger 2
document - safe methods only, generators for path parameters, TODOs for anything
uncertain - and needs --base-url, because a document's servers often name production.
Flags given explicitly win over anything detected.`),
		Example: strings.TrimSpace(`
  tracepoint init --base-url http://127.0.0.1:8080 --path /api/items > tracepoint.yaml
  tracepoint init --base-url '${BASE_URL}' --db postgres --db-dsn-env DATABASE_URL --redis-addr-env REDIS_ADDR --out tracepoint.yaml
  tracepoint init --detect . --out tracepoint.yaml
  tracepoint init --from-openapi api/openapi.yaml --base-url http://127.0.0.1:8080 --output json`),
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			sc, err := ops.Scaffold(in)
			if err != nil {
				return err
			}
			if g.json() {
				return writeJSON(env.Stdout, sc)
			}
			for _, inf := range sc.Inferences {
				fmt.Fprintf(env.Stderr, "found: %s (%s confidence; %s)\n", inf.What, inf.Confidence, inf.Evidence)
			}
			for _, n := range sc.Notes {
				fmt.Fprintln(env.Stderr, "note:", n)
			}
			if out != "" {
				if werr := os.WriteFile(out, []byte(sc.ConfigYAML), 0o600); werr != nil {
					return errs.Wrap(errs.CodeIOWriteFailed, werr, "writing %s", out)
				}
				return nil
			}
			_, err = io.WriteString(env.Stdout, sc.ConfigYAML)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.BaseURL, "base-url", "", "the application's base URL, or an environment reference such as ${BASE_URL}")
	f.StringArrayVar(&in.Paths, "path", nil, "a request path to load; repeatable")
	f.StringVar(&in.DBDriver, "db", "", "probe a database too: postgres, mysql or sqlite")
	f.StringVar(&in.DBDSNEnv, "db-dsn-env", "", "environment variable holding the database DSN")
	f.StringVar(&in.RedisAddrEnv, "redis-addr-env", "", "environment variable holding the Redis address; probes Redis too")
	f.Float64Var(&in.Rate, "rate", 0, "application requests per second (default 20)")
	f.StringVar(&in.Duration, "duration", "", "run length (default 30s)")
	f.StringVar(&in.DBQuery, "db-query", "", "the database probe's query (default SELECT 1, which sees none of the application's tables)")
	f.StringVar(&in.DetectDir, "detect", "", "infer the database, cache, application port and requests from this project directory")
	f.StringVar(&in.OpenAPIPath, "from-openapi", "", "import requests from this OpenAPI 3 or Swagger 2 document (needs --base-url)")
	f.StringVar(&out, "out", "", "write the configuration here instead of stdout")
	return cmd
}

// Capabilities is the machine-readable manifest of this build (§6.1), generated from
// the command tree and the registries rather than written by hand.
type Capabilities struct {
	Tool         buildinfo.Info    `json:"tool"`
	Contracts    map[string]string `json:"contracts"`
	Commands     []CommandInfo     `json:"commands"`
	GlobalFlags  []FlagInfo        `json:"global_flags"`
	ExitCodes    []ExitCodeInfo    `json:"exit_codes"`
	ErrorCodes   []ErrorCodeInfo   `json:"error_codes"`
	Runners      []string          `json:"runners"`
	Drivers      []string          `json:"drivers"`
	Samplers     []string          `json:"samplers"`
	EventTypes   []string          `json:"event_types"`
	Operations   []OperationInfo   `json:"operations"`
	Deprecations []any             `json:"deprecations"`
}

// CommandInfo is one command.
type CommandInfo struct {
	Path    string     `json:"path"`
	Short   string     `json:"short"`
	Args    string     `json:"usage"`
	Example string     `json:"example,omitempty"`
	Flags   []FlagInfo `json:"flags,omitempty"`
}

// FlagInfo is one flag.
type FlagInfo struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

// ExitCodeInfo is one exit code.
type ExitCodeInfo struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

// ErrorCodeInfo is one error code.
type ErrorCodeInfo struct {
	Code      string `json:"code"`
	ExitCode  int    `json:"exit_code"`
	Retriable bool   `json:"retriable"`
}

// OperationInfo is one agent operation.
type OperationInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Annotations ops.Annotations `json:"annotations"`
	CLI         string          `json:"cli,omitempty"`
	MCPTool     string          `json:"mcp_tool"`
	RESTPath    string          `json:"rest_path"`
	Input       any             `json:"input_schema"`
	Output      any             `json:"output_schema"`
}

func newCapabilitiesCmd(env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Describe everything this build can do, for tools and agents",
		Long: strings.TrimSpace(`
Print a manifest of this build: every command and flag with its type and default, exit
codes, error codes, runners, drivers, samplers, event types, agent operations with their
schemas, contract versions and deprecations. It is generated from the command tree and
the registries, so it cannot disagree with the binary.`),
		Example: "  tracepoint capabilities --output json | jq '.operations[].name'",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			caps := BuildCapabilities(cmd.Root())
			if g.json() {
				return writeJSON(env.Stdout, caps)
			}
			fmt.Fprintf(env.Stdout, "tracepoint %s\n\ncommands:\n", caps.Tool.Version)
			for _, c := range caps.Commands {
				fmt.Fprintf(env.Stdout, "  %-16s %s\n", c.Path, c.Short)
			}
			fmt.Fprintln(env.Stdout, "\nagent operations (MCP tools and REST endpoints):")
			for _, o := range caps.Operations {
				fmt.Fprintf(env.Stdout, "  %-16s %s\n", o.Name, o.Title)
			}
			fmt.Fprintf(env.Stdout, "\nrunners %v · drivers %v · samplers %v\n", caps.Runners, caps.Drivers, caps.Samplers)
			return nil
		},
	}
}

// BuildCapabilities walks the command tree and the registries.
func BuildCapabilities(root *cobra.Command) Capabilities {
	caps := Capabilities{
		Tool: buildinfo.Get(),
		Contracts: map[string]string{
			"config": fmt.Sprint(config.Version), "policy": fmt.Sprint(policy.Version),
			"result": result.SchemaVersion, "digest": result.DigestSchemaVersion,
			"events": fmt.Sprint(events.Version), "error": "1",
		},
		Runners:      runner.Registered(),
		Drivers:      sqlrun.Drivers(),
		Samplers:     append([]string{"generator"}, telemetry.Registered()...),
		EventTypes:   []string{events.RunStarted, events.PreflightCompleted, events.BucketSealed, events.IncidentOpened, events.IncidentClosed, events.LevelStarted, events.LevelCompleted, events.Warning, events.RunFinished},
		Deprecations: []any{},
		ExitCodes: []ExitCodeInfo{
			{errs.ExitOK, "success"}, {errs.ExitBreach, "SLO breach or regression"},
			{errs.ExitUsage, "usage, configuration or policy refusal"},
			{errs.ExitRuntime, "preflight or runtime failure, or cut short"},
			{errs.ExitInvalid, "run invalid: the generator, not the target, set the pace"},
			{errs.ExitWaitOpen, "wait timed out while the run continues"},
		},
	}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) { caps.GlobalFlags = append(caps.GlobalFlags, flagInfo(f)) })
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			if child.Hidden || child.Name() == "help" || child.Name() == "completion" {
				continue
			}
			info := CommandInfo{Path: strings.TrimPrefix(child.CommandPath(), root.Name()+" "), Short: child.Short, Args: child.UseLine(), Example: child.Example}
			child.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) { info.Flags = append(info.Flags, flagInfo(f)) })
			caps.Commands = append(caps.Commands, info)
			walk(child)
		}
	}
	walk(root)
	sort.Slice(caps.Commands, func(i, j int) bool { return caps.Commands[i].Path < caps.Commands[j].Path })
	for _, c := range errs.Codes() {
		e := errs.New(c, "")
		caps.ErrorCodes = append(caps.ErrorCodes, ErrorCodeInfo{Code: string(c), ExitCode: e.ExitCode(), Retriable: e.Retriable()})
	}
	for _, op := range ops.Registry() {
		caps.Operations = append(caps.Operations, OperationInfo{
			Name: op.Name, Title: op.Title, Description: op.Description, Annotations: op.Annotations, CLI: op.CLI,
			MCPTool: op.Name, RESTPath: "POST " + rest.Prefix + op.Name, Input: op.Input, Output: op.Output,
		})
	}
	return caps
}

func flagInfo(f *pflag.Flag) FlagInfo {
	return FlagInfo{Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: f.DefValue, Usage: f.Usage}
}
