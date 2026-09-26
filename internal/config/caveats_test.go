package config_test

import "testing"

func TestTrivialProbeIsACaveat(t *testing.T) {
	doc := func(sql string) string {
		return `
version: 1
run: { duration: 10s }
db:
  driver: postgres
  dsn: postgres://x
  executor: { rate: 5 }
  queries: [{ name: q, sql: "` + sql + `" }]
`
	}
	for sql, want := range map[string]bool{
		"SELECT 1": true, "select 1;": true, "SELECT 1 FROM DUAL": true,
		"SELECT title FROM items WHERE id = 1": false, "SELECT 10": false,
	} {
		caveats := mustLoad(t, doc(sql), nil).Caveats()
		if got := len(caveats) == 1 && caveats[0].Code == "PROBE_TRIVIAL" && caveats[0].Severity == "info"; got != want {
			t.Errorf("%q: caveats %+v", sql, caveats)
		}
	}
	if c := mustLoad(t, minimal, nil).Caveats(); len(c) != 0 {
		t.Fatalf("no db, caveats %+v", c)
	}
}
