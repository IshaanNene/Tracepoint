package ops

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/policy"
	"github.com/IshaanNene/Tracepoint/internal/runstore"
	"github.com/IshaanNene/Tracepoint/internal/session"
)

// Service is what every operation runs against. It is built once, when a server
// starts, and nothing a call supplies can change it: the policy in particular is
// fixed here, so no operation can exceed what a human granted (§6.5).
type Service struct {
	Policy policy.Policy
	Store  *runstore.Store
	Lookup func(string) (string, bool)
	// Launch starts a prepared run in the background. It defaults to a detached
	// process, so runs outlive the server that started them.
	Launch func(ctx context.Context, p *session.Prepared) (pid int, err error)
	// Root confines config_path: a caller may name files inside it and nowhere else.
	Root string
	// Actor names the caller in the audit log, such as mcp:claude-code or rest.
	Actor  string
	Logger *slog.Logger
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func (s *Service) launch(ctx context.Context, p *session.Prepared) (int, error) {
	if s.Launch != nil {
		return s.Launch(ctx, p)
	}
	return p.Detach(ctx, "")
}

// ConfigInput names a configuration: inline, or by a path inside the server's root.
type ConfigInput struct {
	Config     any      `json:"config,omitempty" jsonschema:"The configuration itself: a YAML string, or the same document as a JSON object. Give this or config_path, not both. tracepoint schema config describes every field; scaffold_config writes a starter."`
	ConfigPath string   `json:"config_path,omitempty" jsonschema:"Path to a configuration file, relative to the directory the server was started in. Give this or config, not both."`
	Overrides  []string `json:"overrides,omitempty" jsonschema:"--set style overrides applied on top, as path=value; list items are addressable by name, as in db.queries[by-id].weight=5. A digest's recommendations carry these ready to use."`
}

// source reads the named configuration.
func (s *Service) source(in ConfigInput) ([]byte, string, error) {
	switch {
	case in.Config != nil && in.ConfigPath != "":
		return nil, "", errs.New(errs.CodeOpsInvalidInput, "give config or config_path, not both")
	case in.Config != nil:
		if text, ok := in.Config.(string); ok {
			return []byte(text), "inline", nil
		}
		// JSON is YAML, so an object is passed through as a document.
		b, err := json.Marshal(in.Config)
		if err != nil {
			return nil, "", errs.Wrap(errs.CodeOpsInvalidInput, err, "encoding the inline configuration")
		}
		return b, "inline", nil
	case in.ConfigPath != "":
		p, err := s.confine(in.ConfigPath)
		if err != nil {
			return nil, "", err
		}
		b, err := os.ReadFile(p) //nolint:gosec // confined to the server's root above
		if err != nil {
			return nil, "", errs.Wrap(errs.CodeConfigNotFound, err, "no configuration at %s", in.ConfigPath).
				WithHint("config_path is relative to the directory the server was started in")
		}
		return b, in.ConfigPath, nil
	default:
		return nil, "", errs.New(errs.CodeOpsInvalidInput, "no configuration was given").
			WithHint("pass config (YAML text or an object) or config_path; scaffold_config writes a starter")
	}
}

// confine resolves a path inside Root and refuses anything that escapes it, including
// through a symbolic link.
func (s *Service) confine(rel string) (string, error) {
	root := s.Root
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", errs.Wrap(errs.CodeInternal, err, "resolving the server root")
	}
	if filepath.IsAbs(rel) {
		return "", errs.New(errs.CodeOpsInvalidInput, "config_path must be relative to the server's directory").
			WithPath("/config_path")
	}
	full := filepath.Join(absRoot, filepath.Clean(rel))
	if resolved, err := filepath.EvalSymlinks(full); err == nil {
		full = resolved
	}
	if realRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = realRoot
	}
	if full != absRoot && !strings.HasPrefix(full, absRoot+string(filepath.Separator)) {
		return "", errs.New(errs.CodeOpsInvalidInput, "config_path leaves the server's directory").
			WithPath("/config_path").
			WithHint("name a file inside the directory the server was started in, or pass the configuration inline")
	}
	return full, nil
}

// request builds a session request under the server's policy.
func (s *Service) request(in ConfigInput) (session.Request, error) {
	src, path, err := s.source(in)
	if err != nil {
		return session.Request{}, err
	}
	return session.Request{
		Source: src, SourcePath: path, Overrides: in.Overrides, Granted: s.Policy,
		Lookup: s.Lookup, Actor: s.Actor, Logger: s.logger(), Store: s.Store,
	}, nil
}

// run opens a run by id, prefix, suffix or directory.
func (s *Service) run(id string) (*runstore.Run, error) {
	return s.Store.Get(id)
}
