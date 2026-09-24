// Package rest exposes the operation registry as a JSON API with a generated OpenAPI
// 3.1 document, for agent frameworks and services that do not speak MCP. Like the MCP
// adapter it holds no logic: each endpoint is an operation from internal/ops.
//
// Every operation is POST /v1/ops/{name} with the operation's input as the JSON body;
// a success is 200 with its output, a failure the error envelope with a status that
// matches its class.
package rest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/adapters/guard"
	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/ops"
	"github.com/IshaanNene/Tracepoint/internal/schemas"
)

// Paths.
const (
	Prefix      = "/v1/ops/"
	OpenAPIPath = "/v1/openapi.json"
	HealthPath  = "/v1/healthz"
)

// maxBody bounds a request body. A configuration is a few kilobytes.
const maxBody = 1 << 20

// Handler serves every operation. Wrap it with the guard before serving.
func Handler(svc *ops.Service) http.Handler {
	mux := http.NewServeMux()
	for _, op := range ops.Registry() {
		op := op
		mux.HandleFunc("POST "+Prefix+op.Name, func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				guard.WriteError(w, http.StatusRequestEntityTooLarge,
					errs.Wrap(errs.CodeOpsInvalidInput, err, "the request body is too large or unreadable"))
				return
			}
			call := *svc
			call.Actor = "rest"
			out, err := op.Invoke(r.Context(), &call, body)
			if err != nil {
				guard.WriteError(w, Status(err), err)
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	mux.HandleFunc("GET "+OpenAPIPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, OpenAPI())
	})
	mux.HandleFunc("GET "+HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": buildinfo.Get().Version})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		guard.WriteError(w, http.StatusNotFound, errs.New(errs.CodeOpsUnknownOperation, "no endpoint %s %s", r.Method, errs.CleanUntrusted(r.URL.Path)).
			WithHint("operations are POST "+Prefix+"<name>; "+OpenAPIPath+" lists them"))
	})
	return mux
}

// Status maps an error's code to an HTTP status.
func Status(err error) int {
	var typed *errs.Error
	if !errors.As(err, &typed) {
		return http.StatusInternalServerError
	}
	c := string(typed.Code)
	switch {
	case c == string(errs.CodeRunNotFound), c == string(errs.CodeResultNotFound):
		return http.StatusNotFound
	case c == string(errs.CodePolicyTooManyRuns), c == string(errs.CodeRunAlreadyRunning):
		return http.StatusConflict
	case strings.HasPrefix(c, "POLICY_"):
		return http.StatusForbidden
	case strings.HasPrefix(c, "CONFIG_"), strings.HasPrefix(c, "SET_"), strings.HasPrefix(c, "OPS_"), strings.HasPrefix(c, "RESULT_"):
		return http.StatusBadRequest
	case strings.HasPrefix(c, "PREFLIGHT_"):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	b, err := json.Marshal(v)
	if err != nil {
		guard.WriteError(w, http.StatusInternalServerError, errs.Wrap(errs.CodeInternal, err, "encoding the response"))
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(append(b, '\n')); err != nil {
		return // the client has gone; there is no one left to tell
	}
}

// OpenAPI generates the OpenAPI 3.1 document from the registry.
func OpenAPI() map[string]any {
	var errSchema any
	if b, err := schemas.Get(schemas.Error); err == nil {
		if err := json.Unmarshal(b, &errSchema); err != nil {
			errSchema = map[string]any{"type": "object"}
		}
	}
	paths := map[string]any{}
	for _, op := range ops.Registry() {
		paths[Prefix+op.Name] = map[string]any{
			"post": map[string]any{
				"operationId": op.Name,
				"summary":     op.Title,
				"description": op.Description,
				"x-annotations": map[string]bool{
					"read_only": op.Annotations.ReadOnly, "idempotent": op.Annotations.Idempotent,
					"destructive": op.Annotations.Destructive, "open_world": op.Annotations.OpenWorld,
				},
				"requestBody": map[string]any{
					"required": true,
					"content":  map[string]any{"application/json": map[string]any{"schema": op.Input}},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": op.Title,
						"content":     map[string]any{"application/json": map[string]any{"schema": op.Output}},
					},
					"default": map[string]any{
						"description": "A coded error; branch on error.code, never on the message.",
						"content":     map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}},
					},
				},
			},
		}
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "TracePoint",
			"version":     buildinfo.Get().Version,
			"description": "Correlated HTTP, SQL and Redis load testing that reports which tier a slowdown came from. Every operation is also an MCP tool with the same name, input and output.",
		},
		"servers":    []any{map[string]any{"url": "/"}},
		"security":   []any{map[string]any{"bearer": []any{}}},
		"paths":      paths,
		"components": map[string]any{"schemas": map[string]any{"Error": errSchema}, "securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}}},
	}
}
