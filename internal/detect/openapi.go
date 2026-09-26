package detect

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Request is one request a starter configuration should load.
type Request struct {
	Name   string
	Method string
	URL    string
	// Todo, when set, is what a human must check before trusting the request.
	Todo string
}

// OpenAPIOptions control an import.
type OpenAPIOptions struct {
	// Methods are the methods imported; the default is the safe ones, GET and HEAD.
	// Anything that writes needs a human's approval, so it is listed, not imported.
	Methods []string
	// MaxRequests bounds the import; the rest are counted in a note. Default 50.
	MaxRequests int
}

// Import is what an OpenAPI document yields.
type Import struct {
	Title    string
	Servers  []string
	Requests []Request
	Notes    []string
}

var (
	nameUnsafe = regexp.MustCompile(`[^a-z0-9]+`)
	pathParam  = regexp.MustCompile(`\{([^{}/]+)\}`)
)

// ImportOpenAPI turns an OpenAPI 3.x or Swagger 2.0 document, YAML or JSON, into
// requests. Path parameters get generators inferred from their schemas; a required
// query parameter gets one too; anything the document does not pin down becomes a TODO
// rather than a guess presented as fact.
func ImportOpenAPI(data []byte, opts OpenAPIOptions) (*Import, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, errs.Wrap(errs.CodeConfigParse, err, "the OpenAPI document is not valid YAML or JSON")
	}
	version := str(doc["openapi"])
	swagger := str(doc["swagger"])
	if version == "" && swagger == "" {
		return nil, errs.New(errs.CodeConfigInvalidValue, "this is not an OpenAPI document: it has neither an openapi nor a swagger field")
	}
	if len(opts.Methods) == 0 {
		opts.Methods = []string{"get", "head"}
	}
	allowed := map[string]bool{}
	for _, m := range opts.Methods {
		allowed[strings.ToLower(m)] = true
	}
	if opts.MaxRequests <= 0 {
		opts.MaxRequests = 50
	}

	out := &Import{}
	if info, ok := doc["info"].(map[string]any); ok {
		out.Title = str(info["title"])
	}
	basePath := ""
	if swagger != "" {
		basePath = strings.TrimRight(str(doc["basePath"]), "/")
		if host := str(doc["host"]); host != "" {
			scheme := "https"
			if schemes, ok := doc["schemes"].([]any); ok && len(schemes) > 0 {
				scheme = str(schemes[0])
			}
			out.Servers = append(out.Servers, scheme+"://"+host+basePath)
		}
	} else if servers, ok := doc["servers"].([]any); ok {
		for _, s := range servers {
			if m, ok := s.(map[string]any); ok && str(m["url"]) != "" {
				out.Servers = append(out.Servers, str(m["url"]))
			}
		}
	}
	globalAuth := hasSecurity(doc["security"])

	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		paths = nil
	}
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var skipped, deprecated, dropped int
	used := map[string]bool{}
	for _, path := range keys {
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		shared := params(doc, item["parameters"])
		for _, method := range []string{"get", "head", "options", "post", "put", "patch", "delete"} {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			if !allowed[method] {
				skipped++
				continue
			}
			if b, isBool := op["deprecated"].(bool); isBool && b {
				deprecated++
				continue
			}
			if len(out.Requests) == opts.MaxRequests {
				dropped++
				continue
			}
			req, todos := buildRequest(doc, method, basePath+path, op, shared)
			if auth := op["security"]; auth != nil {
				if hasSecurity(auth) {
					todos = append(todos, "needs authentication: add an Authorization header from an environment reference")
				}
			} else if globalAuth {
				todos = append(todos, "needs authentication: add an Authorization header from an environment reference")
			}
			req.Name = uniqueName(requestName(op, method, path), used)
			req.Todo = strings.Join(todos, "; ")
			out.Requests = append(out.Requests, req)
		}
	}
	if skipped > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d operation(s) that write were left out: loading them needs a human's approval and allow_writes in the policy", skipped))
	}
	if deprecated > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d deprecated operation(s) were left out", deprecated))
	}
	if dropped > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d more operation(s) beyond the first %d were left out; pick the ones that matter", dropped, opts.MaxRequests))
	}
	if len(out.Requests) > 0 {
		out.Notes = append(out.Notes, "every request has weight 1; set the weights to the production mix")
	}
	return out, nil
}

// param is one parameter, reduced to what a generator needs.
type param struct {
	name     string
	in       string
	required bool
	schema   map[string]any
}

func params(doc map[string]any, raw any) []param {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []param
	for _, p := range list {
		m, ok := resolve(doc, p).(map[string]any)
		if !ok {
			continue
		}
		pr := param{name: str(m["name"]), in: str(m["in"])}
		if req, isBool := m["required"].(bool); isBool {
			pr.required = req
		}
		if s, ok := resolve(doc, m["schema"]).(map[string]any); ok {
			pr.schema = s
		} else if t := str(m["type"]); t != "" {
			// Swagger 2.0 puts the type on the parameter itself.
			pr.schema = m
		}
		out = append(out, pr)
	}
	return out
}

func buildRequest(doc map[string]any, method, path string, op map[string]any, shared []param) (Request, []string) {
	all := append(append([]param(nil), shared...), params(doc, op["parameters"])...)
	var todos []string
	url := path
	declared := map[string]bool{}
	for _, p := range all {
		if p.in == "path" {
			declared[p.name] = true
		}
	}
	// A path parameter the document never describes still gets a value, so the braces
	// are never sent literally - and a TODO, because the value is a guess.
	for _, m := range pathParam.FindAllStringSubmatch(path, -1) {
		if !declared[m[1]] {
			all = append(all, param{name: m[1], in: "path"})
		}
	}
	var query []string
	for _, p := range all {
		switch p.in {
		case "path":
			gen, todo := generator(p)
			url = strings.ReplaceAll(url, "{"+p.name+"}", gen)
			if todo != "" {
				todos = append(todos, todo)
			}
		case "query":
			if !p.required {
				continue
			}
			gen, todo := generator(p)
			query = append(query, p.name+"="+gen)
			if todo != "" {
				todos = append(todos, todo)
			}
		case "header":
			if p.required {
				todos = append(todos, fmt.Sprintf("requires header %s", p.name))
			}
		}
	}
	if len(query) > 0 {
		url += "?" + strings.Join(query, "&")
	}
	return Request{Method: strings.ToUpper(method), URL: url}, todos
}

// generator picks a template generator for a parameter from its schema.
func generator(p param) (string, string) {
	s := p.schema
	if s == nil {
		return "{{randString 8}}", fmt.Sprintf("parameter %s has no schema; replace it with values that exist", p.name)
	}
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 {
		vals := make([]string, 0, len(enum))
		for _, v := range enum {
			vals = append(vals, fmt.Sprint(v))
		}
		return "{{pick " + strings.Join(vals, "|") + "}}", ""
	}
	switch str(s["type"]) {
	case "integer", "number":
		lo, hi := 1, 1000
		if v, ok := number(s["minimum"]); ok {
			lo = int(v)
		}
		if v, ok := number(s["maximum"]); ok && int(v) >= lo {
			hi = int(v)
		}
		todo := ""
		if _, bounded := number(s["maximum"]); !bounded {
			todo = fmt.Sprintf("parameter %s draws ids 1-1000; narrow it to ids that exist", p.name)
		}
		return fmt.Sprintf("{{randInt %d %d}}", lo, hi), todo
	case "boolean":
		return "{{pick true|false}}", ""
	case "string":
		if str(s["format"]) == "uuid" {
			return "{{uuid}}", fmt.Sprintf("parameter %s is a random uuid, which will rarely exist; use a feeder of real ids", p.name)
		}
		return "{{randString 8}}", fmt.Sprintf("parameter %s is a free-form string; replace it with values that exist, or a feeder", p.name)
	}
	return "{{randString 8}}", fmt.Sprintf("parameter %s has an unusual schema; replace it with values that exist", p.name)
}

func requestName(op map[string]any, method, path string) string {
	if id := str(op["operationId"]); id != "" {
		return strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(id), "-"), "-")
	}
	name := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(path), "-"), "-")
	if name == "" {
		name = "root"
	}
	if method != "get" {
		name = method + "-" + name
	}
	return name
}

func uniqueName(name string, used map[string]bool) string {
	candidate := name
	for i := 2; used[candidate]; i++ {
		candidate = fmt.Sprintf("%s-%d", name, i)
	}
	used[candidate] = true
	return candidate
}

// resolve follows a local $ref, once per level, with a bound so a cycle cannot loop.
func resolve(doc map[string]any, v any) any {
	for range 8 {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref := str(m["$ref"])
		if !strings.HasPrefix(ref, "#/") {
			return v
		}
		var cur any = doc
		for _, part := range strings.Split(ref[2:], "/") {
			cm, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = cm[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
		}
		v = cur
	}
	return nil
}

func hasSecurity(v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && len(m) > 0 {
			return true
		}
	}
	return false
}

func str(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprint(s)
	}
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
