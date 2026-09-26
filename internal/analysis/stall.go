package analysis

import (
	"fmt"
	"sort"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// generatorStalls finds the buckets in which the whole generator was paused:
//
//   - every runner with usable data in the bucket saw its client wait - the time a
//     request spent inside TracePoint before it could be sent - jump to at least
//     StallRatio times its own median and StallAbsMS above it;
//   - no runner's service time was over its absolute threshold there by more than a
//     pause could explain: a pause adds to service time about as much as it adds to
//     client wait, so a rise more than StallServiceFactor times the bucket's largest
//     client-wait rise is the target, not the pause; and
//   - the bucket is part of a run of at most StallMaxBuckets such buckets.
//
// That is what a process descheduled by its host looks like from inside: brief, felt
// by every runner, and not a target misbehaving. A saturated runner raises only its
// own client wait. A real storage stall perturbs the generator as well - blocked
// workers pile up and every runner's client wait rises - but it lasts, and a tier is
// far over its threshold meanwhile, so the last two conditions keep it an incident.
// It takes at least StallMinRunners runners to tell the difference, so a
// single-runner run never has stalls.
func generatorStalls(runners []result.Runner, thresholds map[string]result.Threshold, p Params) []int {
	type baseline struct {
		byIndex   map[int]*result.Bucket
		median    float64 // client wait
		service   float64 // service time
		threshold float64
	}
	var bases []baseline
	for i := range runners {
		rn := &runners[i]
		b := baseline{byIndex: map[int]*result.Bucket{}, threshold: thresholds[rn.Name].ThresholdMS}
		var waits, services []float64
		for j := range rn.Buckets {
			bk := &rn.Buckets[j]
			if eligible(bk) {
				b.byIndex[bk.Index] = bk
				waits = append(waits, bk.ClientWait.P99)
				services = append(services, bk.Service.P99)
			}
		}
		if len(waits) == 0 {
			continue
		}
		b.median, b.service = median(waits), median(services)
		bases = append(bases, b)
	}
	if len(bases) < p.StallMinRunners {
		return nil
	}

	indexes := map[int]bool{}
	for _, b := range bases {
		for i := range b.byIndex {
			indexes[i] = true
		}
	}
	var candidates []int
	for i := range indexes {
		present, raised := 0, 0
		pause := 0.0 // the largest client-wait rise: how long the pause was, roughly
		for _, b := range bases {
			bk, ok := b.byIndex[i]
			if !ok {
				continue
			}
			present++
			rise := bk.ClientWait.P99 - b.median
			if bk.ClientWait.P99 >= p.StallRatio*b.median && rise >= p.StallAbsMS {
				raised++
			}
			pause = max(pause, rise)
		}
		targetMoved := false
		for _, b := range bases {
			if bk, ok := b.byIndex[i]; ok && b.threshold > 0 && bk.Service.P99 > b.threshold &&
				bk.Service.P99-b.service > p.StallServiceFactor*pause {
				targetMoved = true
			}
		}
		if present >= p.StallMinRunners && raised == present && !targetMoved {
			candidates = append(candidates, i)
		}
	}
	sort.Ints(candidates)

	// Keep only short runs: a pause is brief, and one that seems to last is something
	// else that happened to hold every runner up.
	var out []int
	for start := 0; start < len(candidates); {
		end := start
		for end+1 < len(candidates) && candidates[end+1] == candidates[end]+1 {
			end++
		}
		if end-start+1 <= p.StallMaxBuckets {
			out = append(out, candidates[start:end+1]...)
		}
		start = end + 1
	}
	return out
}

// stallFinding reports stalled buckets. It degrades the run: the pauses are the
// host's, and they are left out of every hot-bucket judgement, but a reader should
// know the generator was not running freely.
func stallFinding(stalls []int) result.Finding {
	return result.Finding{
		Code:     CodeGeneratorStall,
		Severity: result.SeverityWarn,
		Message: fmt.Sprintf("the generator paused in %d bucket(s): every runner's requests left late at once, so those buckets describe this machine, not the target, and were left out of the analysis",
			len(stalls)),
		Detail: map[string]any{"buckets": stalls},
		Fix:    "run the generator on a host with dedicated CPU; on a shared or virtualised host, brief pauses are expected",
	}
}
