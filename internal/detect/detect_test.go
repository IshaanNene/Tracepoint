package detect_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/detect"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestDetectAProject(t *testing.T) {
	d, err := detect.Detect(filepath.Join("testdata", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	if d.BaseURL != "http://127.0.0.1:8080" || d.DBDriver != "postgres" || d.DBDSNEnv != "DATABASE_URL" || d.RedisAddrEnv != "REDIS_ADDR" {
		t.Fatalf("detection = %+v", d)
	}
	if d.OpenAPI != filepath.Join("api", "openapi.yaml") || d.Import == nil || len(d.Import.Requests) != 5 {
		t.Fatalf("openapi %q, import %+v", d.OpenAPI, d.Import)
	}
	byWhat := map[string]string{}
	for _, inf := range d.Inferences {
		byWhat[inf.What] = inf.Confidence
		if inf.Evidence == "" {
			t.Errorf("an inference without evidence: %+v", inf)
		}
	}
	if byWhat["a Postgres database"] != detect.High || byWhat["the application at http://127.0.0.1:8080"] != detect.High {
		t.Fatalf("inferences %v", byWhat)
	}
	// Values are never read: a secret in the compose file or an env file cannot reach
	// anything detection returns.
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "detect-sentinel") {
		t.Fatal("a value from an environment file reached the detection")
	}
}

func TestDetectAnEmptyDirectory(t *testing.T) {
	d, err := detect.Detect(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.BaseURL != "${BASE_URL}" || d.DBDriver != "" || len(d.Notes) == 0 {
		t.Fatalf("detection = %+v", d)
	}
	if _, err := detect.Detect(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing directory was accepted")
	}
}

func TestImportOpenAPI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "shop", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	imp, err := detect.ImportOpenAPI(b, detect.OpenAPIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]detect.Request{}
	for _, r := range imp.Requests {
		got[r.Name] = r
		if r.Method != "GET" {
			t.Errorf("%s: method %s; only safe methods are imported", r.Name, r.Method)
		}
	}
	for name, want := range map[string]struct{ url, todo string }{
		"listitems":            {"/items", ""},
		"getitem":              {"/items/{{randInt 1 500}}", "needs authentication"},
		"users-uid":            {"/users/{{uuid}}", "random uuid"},
		"search":               {"/search?q={{randString 8}}&sort={{pick asc|desc}}", "free-form string"},
		"orders-orderid-lines": {"/orders/{{randString 8}}/lines", "no schema"},
	} {
		r, ok := got[name]
		if !ok {
			t.Errorf("no request %s in %v", name, imp.Requests)
			continue
		}
		if r.URL != want.url || (want.todo == "") != (r.Todo == "") || !strings.Contains(r.Todo, want.todo) {
			t.Errorf("%s: url %q todo %q; want %q and a todo containing %q", name, r.URL, r.Todo, want.url, want.todo)
		}
	}
	notes := strings.Join(imp.Notes, " | ")
	if !strings.Contains(notes, "2 operation(s) that write were left out") || !strings.Contains(notes, "1 deprecated") {
		t.Fatalf("notes %s", notes)
	}
	if len(imp.Servers) != 1 || imp.Title != "Shop" {
		t.Fatalf("servers %v title %q", imp.Servers, imp.Title)
	}
}

func TestImportSwagger2AndJSON(t *testing.T) {
	doc := `{"swagger":"2.0","host":"api.example","basePath":"/v1","schemes":["http"],
	  "paths":{"/pets/{petId}":{"get":{"parameters":[{"name":"petId","in":"path","required":true,"type":"integer"}]}}}}`
	imp, err := detect.ImportOpenAPI([]byte(doc), detect.OpenAPIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Requests) != 1 || imp.Requests[0].URL != "/v1/pets/{{randInt 1 1000}}" || imp.Servers[0] != "http://api.example/v1" {
		t.Fatalf("import %+v", imp)
	}
	if _, err := detect.ImportOpenAPI([]byte("title: not openapi"), detect.OpenAPIOptions{}); err == nil {
		t.Fatal("a non-OpenAPI document was accepted")
	}
}
