package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
)

func newValidateCmd(env Env, g *globals) *cobra.Command {
	var (
		configPath string
		policyPath string
		overrides  []string
	)

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check a configuration without running anything",
		Long: strings.TrimSpace(`
Load, interpolate and validate a configuration, reporting every problem it has rather
than only the first. Nothing is contacted and no load is generated, so this is safe to
run against a production configuration.`),
		Example: strings.TrimSpace(`
  tracepoint validate -c tracepoint.yaml
  tracepoint validate -c tracepoint.yaml --output json`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, warnings, err := prepare(cmd.Context(), env, configPath, policyPath, overrides)
			if err != nil {
				return err
			}
			if g.json() {
				return writeJSON(env.Stdout, map[string]any{
					"valid":     true,
					"path":      configPath,
					"runners":   cfg.Runners(),
					"duration":  cfg.Run.Duration.String(),
					"targets":   cfg.Targets(),
					"warnings":  warnings,
					"effective": cfg.Redacted(),
				})
			}
			fmt.Fprintf(env.Stdout, "%s is valid: %s over %s\n",
				configPath, strings.Join(cfg.Runners(), ", "), cfg.Run.Duration)
			for _, w := range warnings {
				fmt.Fprintf(env.Stdout, "  ! %s\n", w.Message)
				if w.Fix != "" {
					fmt.Fprintf(env.Stdout, "    %s\n", w.Fix)
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&configPath, "config", "c", "tracepoint.yaml", "configuration file, or - to read standard input")
	f.StringVar(&policyPath, "policy", "", "safety policy file to validate against")
	f.StringArrayVar(&overrides, "set", nil, "override a configuration value, as path=value")
	return cmd
}

func newSchemaCmd(env Env) *cobra.Command {
	names := make([]string, 0, len(schemas.Names()))
	lines := make([]string, 0, len(schemas.Names()))
	for _, n := range schemas.Names() {
		names = append(names, string(n))
		lines = append(lines, fmt.Sprintf("  %-8s %s", n, schemas.Describe(n)))
	}

	return &cobra.Command{
		Use:   "schema <" + strings.Join(names, "|") + ">",
		Short: "Print a JSON Schema for one of TracePoint's contracts",
		Long: strings.TrimSpace(`
Print the JSON Schema for one of TracePoint's contract documents. The schemas are
embedded in the binary, so what is printed is exactly what this build produces and
validates against - they cannot disagree.

` + strings.Join(lines, "\n")),
		Example: strings.TrimSpace(`
  tracepoint schema config > config.schema.json
  tracepoint schema result | jq '.properties.analysis'`),
		Args:      cobra.ExactArgs(1),
		ValidArgs: names,
		RunE: func(_ *cobra.Command, args []string) error {
			doc, err := schemas.Get(schemas.Name(args[0]))
			if err != nil {
				return err
			}
			if _, err := env.Stdout.Write(doc); err != nil {
				return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the schema")
			}
			return nil
		},
	}
}

func newVersionCmd(env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the version of this build",
		Args:    cobra.NoArgs,
		Example: "  tracepoint version --output json",
		RunE: func(*cobra.Command, []string) error {
			info := buildinfo.Get()
			if g.json() {
				return writeJSON(env.Stdout, info)
			}
			fmt.Fprintf(env.Stdout, "tracepoint %s (%s, built %s, %s %s/%s)\n",
				info.Version, info.Commit, info.Date, info.Go, info.OS, info.Arch)
			return nil
		},
	}
}
