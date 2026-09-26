package analysis

import (
	"strings"
	"testing"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// stallAt makes every runner's client wait jump in one bucket, the way a whole
// generator process paused by its host looks from inside: each runner's requests were
// sent late, and each measured it. The pause also nudges service time up.
func (s *scenario) stallAt(i int, waitMS float64) *scenario {
	for name, vals := range s.runners {
		s.waitAt(name, i, i, waitMS)
		vals[i] *= 4
	}
	return s
}

func findingCodes(a result.Analysis) []string {
	var out []string
	for _, f := range a.Validity.Findings {
		out = append(out, f.Code)
	}
	return out
}

// A one-bucket pause of the whole generator is the generator's, not the target's: no
// hot bucket, no incident, and the run says it was disturbed.
func TestGeneratorStallIsNotAnIncident(t *testing.T) {
	r := newScenario(60).stallAt(30, 20).result()
	a := analyse(t, r)
	if len(a.Incidents) != 0 || len(a.HotBuckets) != 0 {
		t.Fatalf("a stall made incidents %+v, hot %v", a.Incidents, a.HotBuckets)
	}
	var stall *result.Finding
	for i := range a.Validity.Findings {
		if a.Validity.Findings[i].Code == CodeGeneratorStall {
			stall = &a.Validity.Findings[i]
		}
	}
	if stall == nil || stall.Severity != result.SeverityWarn || a.Validity.State != result.ValidityDegraded {
		t.Fatalf("validity = %+v, want a GENERATOR_STALL warning", a.Validity)
	}
	if d, ok := stall.Detail.(map[string]any); !ok || !equalInts(d["buckets"].([]int), []int{30}) {
		t.Fatalf("detail = %+v, want buckets [30]", stall.Detail)
	}
	if a.Verdict.Bottleneck != result.BottleneckNone {
		t.Fatalf("verdict = %s, want none", a.Verdict.Bottleneck)
	}
	r.Analysis = a
	found := false
	for _, rec := range result.BuildDigest(r, result.DigestOptions{}).Recommendations {
		found = found || rec.ID == "dedicated-generator-host"
	}
	if !found {
		t.Fatal("the digest should recommend a host with dedicated CPU")
	}
}

// A stall must not become its runners' baseline either: a real fault right after it
// is still found against the steady buckets before.
func TestStallIsLeftOutOfTheBaseline(t *testing.T) {
	r := newScenario(60).stallAt(29, 20).set("db", 30, 34, 900).set("http", 31, 35, 950).result()
	inc := only(t, analyse(t, r))
	if inc.Class != result.IncidentCorrelated || inc.Culprit == nil || inc.Culprit.Runner != "db" {
		t.Fatalf("incident = %+v", inc)
	}
}

// Only a pause every runner felt is a stall. The app runner alone waiting on its own
// queue is a client-limited run - a finding about the configuration - and stays one.
func TestStallNeedsEveryRunner(t *testing.T) {
	r := newScenario(60).set("http", 30, 34, 300).waitAt("http", 30, 34, 2000).waitAt("db", 30, 30, 20).result()
	a := analyse(t, r)
	if inc := only(t, a); inc.Class != result.IncidentClientLimited {
		t.Fatalf("class = %s, want client_limited", inc.Class)
	}
	for _, c := range findingCodes(a) {
		if c == CodeGeneratorStall {
			t.Fatal("two of three runners waiting is not a stall")
		}
	}
}

// With one runner there is nothing to tell a stall from a queue with, so nothing is
// called a stall.
func TestStallNeedsTwoRunners(t *testing.T) {
	r := newScenario(60).drop("db").drop("redis").set("http", 30, 30, 80).waitAt("http", 30, 30, 20).result()
	for _, c := range findingCodes(analyse(t, r)) {
		if c == CodeGeneratorStall {
			t.Fatal("a single runner cannot show a stall")
		}
	}
}

// A small, steady client wait is not a stall however many runners share it: the rise
// must be both 3x the runner's own median and 5ms above it.
func TestStallNeedsARealJump(t *testing.T) {
	r := newScenario(60).stallAt(30, 3).result()
	for _, c := range findingCodes(analyse(t, r)) {
		if c == CodeGeneratorStall {
			t.Fatal("a 3ms client wait is not a stall")
		}
	}
}

// A storage tier that went hot only after the application, and not past its own
// threshold, is not evidence against it: the incident is reported without a culprit,
// and the verdict says why rather than calling it a tie.
func TestFollowerIsNotACulprit(t *testing.T) {
	r := newScenario(60).set("http", 20, 25, 400).set("redis", 24, 24, 21).result()
	a := analyse(t, r)
	inc := only(t, a)
	if inc.Class != result.IncidentCorrelated || inc.Culprit != nil {
		t.Fatalf("incident = %+v, want correlated with no culprit", inc)
	}
	if a.Verdict.Bottleneck != result.BottleneckInconclusive {
		t.Fatalf("verdict = %s, want inconclusive", a.Verdict.Bottleneck)
	}
	if want := "went hot only after the application"; !strings.Contains(a.Verdict.Summary, want) {
		t.Fatalf("summary = %q, want it to say %q", a.Verdict.Summary, want)
	}
}

// A real storage stall perturbs the generator too - blocked workers pile up and
// every runner's client wait rises - but it lasts, and a tier is far over its
// threshold while it does. Measured from the known-answer suite's Postgres lock:
// five buckets, db service time near 5s, every runner's client wait at 8-45ms.
func TestLongPauseWithAHotTierIsNotAStall(t *testing.T) {
	s := newScenario(60).set("db", 20, 24, 4900)
	for name := range s.runners {
		s.waitAt(name, 20, 24, 20)
	}
	a := analyse(t, s.result())
	for _, c := range findingCodes(a) {
		if c == CodeGeneratorStall {
			t.Fatal("a five-bucket storage stall was called a generator stall")
		}
	}
	if inc := only(t, a); inc.Class != result.IncidentStorageOnly {
		t.Fatalf("class = %s, want storage_only", inc.Class)
	}
}

// Every runner waiting in the same single bucket is still no stall when a tier was
// over its own threshold in it: something real happened to a target.
func TestShortPauseWithATierOverThresholdIsNotAStall(t *testing.T) {
	s := newScenario(60).set("db", 30, 30, 400)
	for name := range s.runners {
		s.waitAt(name, 30, 30, 20)
	}
	for _, c := range findingCodes(analyse(t, s.result())) {
		if c == CodeGeneratorStall {
			t.Fatal("a bucket with db at 4x its threshold was called a stall")
		}
	}
}

// Two consecutive buckets are still a pause - one can straddle a bucket boundary.
func TestStallMayStraddleTwoBuckets(t *testing.T) {
	a := analyse(t, newScenario(60).stallAt(30, 20).stallAt(31, 20).result())
	if len(a.Incidents) != 0 {
		t.Fatalf("a two-bucket pause made incidents: %+v", a.Incidents)
	}
	if !containsCode(findingCodes(a), CodeGeneratorStall) {
		t.Fatal("no stall reported")
	}
}

func containsCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// A long enough pause lands mid-request and pushes service time over a threshold too,
// but only by about as much as the pause itself, which client wait also measures.
// Seen on the known-answer host: a 42ms scheduler stall, db and redis at ~120ms.
func TestPauseThatInflatesServiceTimeIsStillAStall(t *testing.T) {
	s := newScenario(60).set("db", 28, 28, 120).set("redis", 28, 28, 118).set("http", 28, 28, 76)
	for name := range s.runners {
		s.waitAt(name, 28, 28, 45)
	}
	a := analyse(t, s.result())
	if len(a.Incidents) != 0 || !containsCode(findingCodes(a), CodeGeneratorStall) {
		t.Fatalf("incidents %+v, findings %v", a.Incidents, findingCodes(a))
	}
}
