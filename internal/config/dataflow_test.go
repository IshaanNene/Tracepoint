package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// problem returns the one coded problem a document is refused for.
func problem(t *testing.T, doc string) *errs.Error {
	t.Helper()
	_, err := load(t, doc, nil)
	var typed *errs.Error
	if !errors.As(err, &typed) {
		t.Fatalf("want a coded error, got %v", err)
	}
	if len(typed.Causes) > 0 {
		t.Fatalf("want exactly one problem, got %v", typed.Causes)
	}
	return typed
}

const journeyHead = `
version: 1
run: { duration: 10s }
http:
  base_url: http://127.0.0.1:8080
  executor: { rate: 5 }
`

// The dataflow check (§4): every variable a step reads must have been extracted by an
// earlier step of the same journey, so a token can never be sent literally.
func TestDataflowAcceptsWhatEarlierStepsExtract(t *testing.T) {
	mustLoad(t, journeyHead+`
  feeders: [{ name: users, file: users.csv }]
  journeys:
    - name: checkout
      steps:
        - name: login
          method: POST
          url: /login
          body: '{"user":"{{users.email}}"}'
          extract: [{ name: token, path: token }, { name: cart, from: header, path: X-Cart }]
        - name: add
          url: /cart/{{cart}}/items?sku={{randInt 1 9}}
          headers: { Authorization: "Bearer {{token}}" }
`, nil)
}

func TestDataflowRejectsAVariableNothingExtracts(t *testing.T) {
	e := problem(t, journeyHead+`
  journeys:
    - name: checkout
      steps:
        - { name: view, url: "/items/{{item_id}}" }
`)
	if e.Code != errs.CodeConfigUndefinedVariable || e.Path != "/http/journeys/0/steps/0/url" || !strings.Contains(e.Message, "item_id") {
		t.Fatalf("got %s at %s: %s", e.Code, e.Path, e.Message)
	}
}

// Extraction happens after the response, so a step cannot use what it extracts itself,
// and order matters: a later step's extraction is not available earlier.
func TestDataflowRequiresAnEarlierStep(t *testing.T) {
	e := problem(t, journeyHead+`
  journeys:
    - name: checkout
      steps:
        - name: add
          url: /cart/{{cart}}
          extract: [{ name: cart, path: id }]
`)
	if e.Code != errs.CodeConfigUndefinedVariable || !strings.Contains(e.Hint, "earlier step") {
		t.Fatalf("got %s: %s / %s", e.Code, e.Message, e.Hint)
	}
	e = problem(t, journeyHead+`
  journeys:
    - name: checkout
      steps:
        - { name: view, url: "/items/{{id}}" }
        - { name: list, url: /items, extract: [{ name: id, path: "0.id" }] }
`)
	if e.Code != errs.CodeConfigUndefinedVariable {
		t.Fatalf("got %s", e.Code)
	}
}

// Variables do not cross journeys, and a plain request is a journey of one step, so a
// request can never read a variable.
func TestDataflowDoesNotCrossJourneysOrRequests(t *testing.T) {
	e := problem(t, journeyHead+`
  journeys:
    - name: a
      steps: [{ name: s, url: /x, extract: [{ name: tok, path: t }] }]
    - name: b
      steps: [{ name: s, url: "/y?t={{tok}}" }]
`)
	if e.Code != errs.CodeConfigUndefinedVariable || e.Path != "/http/journeys/1/steps/0/url" {
		t.Fatalf("got %s at %s", e.Code, e.Path)
	}
	e = problem(t, journeyHead+`
  requests: [{ name: r, url: "/y", body: "{{tok}}" }]
`)
	if e.Code != errs.CodeConfigUndefinedVariable || e.Path != "/http/requests/0/body" {
		t.Fatalf("got %s at %s", e.Code, e.Path)
	}
}

func TestDataflowFeeders(t *testing.T) {
	e := problem(t, journeyHead+`
  requests: [{ name: r, url: "/users/{{people.id}}" }]
`)
	if e.Code != errs.CodeConfigFeederNotFound || !strings.Contains(e.Message, "people") {
		t.Fatalf("got %s: %s", e.Code, e.Message)
	}
}

// The storage runners have no journeys and no feeders: their arguments may use
// generators only.
func TestDataflowStorageRunnersTakeGeneratorsOnly(t *testing.T) {
	e := problem(t, `
version: 1
run: { duration: 10s }
db:
  driver: postgres
  dsn: postgres://x
  executor: { rate: 5 }
  queries: [{ name: q, sql: "SELECT 1 WHERE $1 = $2", args: ["{{randInt 1 5}}", "{{token}}"] }]
`)
	if e.Code != errs.CodeConfigUndefinedVariable || e.Path != "/db/queries/0/args/1" {
		t.Fatalf("got %s at %s", e.Code, e.Path)
	}
	e = problem(t, `
version: 1
run: { duration: 10s }
redis:
  addr: 127.0.0.1:6379
  executor: { rate: 5 }
  commands: [{ name: c, cmd: [GET, "user:{{users.id}}"] }]
`)
	if e.Code != errs.CodeConfigFeederNotFound || e.Path != "/redis/commands/0/cmd/1" {
		t.Fatalf("got %s at %s", e.Code, e.Path)
	}
}

// A template that does not compile is reported at load, with its path, rather than
// when the runner is built.
func TestDataflowReportsTemplateSyntaxWithAPath(t *testing.T) {
	e := problem(t, journeyHead+`
  requests: [{ name: r, url: "/y/{{randIntt 1 2}}" }]
`)
	if e.Code != errs.CodeConfigTemplateUnknownFn || e.Path != "/http/requests/0/url" {
		t.Fatalf("got %s at %s", e.Code, e.Path)
	}
}
