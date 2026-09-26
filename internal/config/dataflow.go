package config

import (
	"errors"
	"fmt"
	"sort"

	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/template"
)

// Scope is what a template may read at one point in the configuration: the variables
// earlier steps of the same journey extracted, and the declared feeders.
type Scope struct {
	// Owner describes where the template is, for messages: `step "add" of journey
	// "checkout"`, `request "r"`, `query "q"`.
	Owner string
	// Vars are the variables available.
	Vars map[string]bool
	// Feeders are the feeders declared, or nil where feeders cannot be used at all.
	Feeders map[string]bool
	// Journey is true inside a multi-step journey, which changes the advice.
	Journey bool
	// Storage is true for db and redis, which run alone and take no feeders.
	Storage bool
}

// CheckTemplate compiles a template and checks that everything it reads is available
// in scope (§4, the static dataflow check). It is exported so a runner can apply the
// same check to text only it reads, such as a body_file.
func CheckTemplate(source, path string, s Scope) *errs.Error {
	tpl, err := template.Compile(source)
	if err != nil {
		var typed *errs.Error
		if !errors.As(err, &typed) {
			typed = errs.Wrap(errs.CodeConfigTemplateSyntax, err, "%s", err.Error())
		}
		if typed.Path == "" {
			typed = typed.WithPath(path)
		}
		return typed
	}
	for _, v := range tpl.Vars() {
		if s.Vars[v] {
			continue
		}
		e := errs.New(errs.CodeConfigUndefinedVariable, "%s reads {{%s}}, which nothing has extracted", s.Owner, v).WithPath(path)
		switch {
		case s.Storage:
			return e.WithHint("storage runners run each operation alone, so nothing can be extracted for them; use a generator such as {{randInt 1 1000}}")
		case s.Journey:
			return e.WithHint("extract it in an earlier step of the same journey, as extract: [{name: %s, path: ...}], or read it from a feeder as {{feeder.column}}", v)
		default:
			return e.WithHint("a request runs alone, so nothing can have extracted a value for it; make it a journey whose earlier step extracts %s, or use a feeder or a generator", v)
		}
	}
	for _, f := range tpl.Feeders() {
		if s.Feeders[f] {
			continue
		}
		e := errs.New(errs.CodeConfigFeederNotFound, "%s reads from feeder %q, which is not declared", s.Owner, f).WithPath(path)
		if s.Storage {
			return e.WithHint("feeders supply http requests and journeys only; use a generator here")
		}
		return e.WithHint("declare it under http.feeders, as {name: %s, file: data.csv}", f)
	}
	return nil
}

// validateDataflow walks every templated field in the configuration.
func (c *Config) validateDataflow(add func(*errs.Error)) {
	check := func(source, path string, s Scope) {
		if e := CheckTemplate(source, path, s); e != nil {
			add(e)
		}
	}

	if h := c.HTTP; h != nil {
		feeders := map[string]bool{}
		for _, f := range h.Feeders {
			feeders[f.Name] = true
		}
		// Headers set for every request are rendered before any step has run.
		for _, k := range sortedKeys(h.Headers) {
			check(h.Headers[k], "/http/headers/"+k, Scope{Owner: "http.headers", Vars: map[string]bool{}, Feeders: feeders})
		}
		for i := range h.Requests {
			s := &h.Requests[i]
			scope := Scope{Owner: fmt.Sprintf("request %q", s.Name), Vars: map[string]bool{}, Feeders: feeders}
			c.checkStep(s, fmt.Sprintf("/http/requests/%d", i), scope, check)
		}
		for i := range h.Journeys {
			j := &h.Journeys[i]
			vars := map[string]bool{}
			for k := range j.Steps {
				s := &j.Steps[k]
				scope := Scope{
					Owner:   fmt.Sprintf("step %q of journey %q", s.Name, j.Name),
					Vars:    copySet(vars),
					Feeders: feeders, Journey: true,
				}
				c.checkStep(s, fmt.Sprintf("/http/journeys/%d/steps/%d", i, k), scope, check)
				// Only after the step has run can its extractions be read.
				for _, e := range s.Extract {
					vars[e.Name] = true
				}
			}
		}
	}

	if d := c.DB; d != nil {
		for i, q := range d.Queries {
			scope := Scope{Owner: fmt.Sprintf("query %q", q.Name), Storage: true}
			for k, a := range q.Args {
				if text, ok := a.Value.(string); ok {
					check(text, fmt.Sprintf("/db/queries/%d/args/%d", i, k), scope)
				}
			}
			for t, st := range q.Tx {
				for k, a := range st.Args {
					if text, ok := a.Value.(string); ok {
						check(text, fmt.Sprintf("/db/queries/%d/tx/%d/args/%d", i, t, k), scope)
					}
				}
			}
		}
	}

	if r := c.Redis; r != nil {
		for i, cmd := range r.Commands {
			scope := Scope{Owner: fmt.Sprintf("command %q", cmd.Name), Storage: true}
			for k, part := range cmd.Cmd {
				check(part, fmt.Sprintf("/redis/commands/%d/cmd/%d", i, k), scope)
			}
			for p, line := range cmd.Pipeline {
				for k, part := range line {
					check(part, fmt.Sprintf("/redis/commands/%d/pipeline/%d/%d", i, p, k), scope)
				}
			}
		}
	}
}

// checkStep checks one HTTP step's url, body and headers. A body_file is checked by
// the runner when it reads the file.
func (c *Config) checkStep(s *HTTPStep, path string, scope Scope, check func(string, string, Scope)) {
	check(s.URL, path+"/url", scope)
	if s.Body != "" {
		check(s.Body, path+"/body", scope)
	}
	for _, k := range sortedKeys(s.Headers) {
		check(s.Headers[k], path+"/headers/"+k, scope)
	}
}

func copySet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
