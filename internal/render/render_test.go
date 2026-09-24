package render_test

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/internal/render"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestFormatting(t *testing.T) {
	for in, want := range map[float64]string{0: "0", 0.456: "0.46ms", 4.56: "4.6ms", 456: "456ms", 4560: "4.6s", 120_000: "2.0m"} {
		if got := render.MS(in); got != want {
			t.Errorf("MS(%v) = %s, want %s", in, got, want)
		}
	}
	if render.Seconds(3) != "3s" || render.Seconds(2.5) != "2.5s" {
		t.Error("Seconds")
	}
	if render.Number(40) != "40" || render.Number(0.12345) != "0.123" {
		t.Error("Number")
	}
	for in, want := range map[float64]string{0: "0%", 0.0004: "<0.1%", 0.0125: "1.2%", 1: "100.0%"} {
		if got := render.Percent(in); got != want {
			t.Errorf("Percent(%v) = %s, want %s", in, got, want)
		}
	}
	got := render.SortedCounts(map[string]int64{"timeout": 3, "conn_refused": 3, "http_5xx": 9})
	if len(got) != 3 || got[0].Key != "http_5xx" || got[1].Key != "conn_refused" || got[2].Key != "timeout" {
		t.Fatalf("SortedCounts = %+v", got)
	}
}
