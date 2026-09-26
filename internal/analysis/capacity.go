package analysis

import (
	"github.com/IshaanNene/Tracepoint/internal/result"
)

// AttributeLevels names the tier that broke each broken level of a capacity search
// (spec §5.6: "culprit per broken level via §5.5").
//
// A level is judged by the same analysis a whole run gets, over the timeline up to the
// level's end - so the lower levels before it are its baseline, and nothing that
// happened later can reach back into it. The incident that overlaps the level's
// measured window most names the tier: its culprit for a correlated incident, the hot
// storage tier for a storage-only one, the application for an app-only one. A level
// with no such incident - one that broke on a plateau with nothing going hot, or on
// the generator's side - names none.
func AttributeLevels(r *result.Result, in Inputs) {
	if r.Capacity == nil {
		return
	}
	for i := range r.Capacity.Levels {
		l := &r.Capacity.Levels[i]
		if l.OK {
			continue
		}
		l.Culprit = levelCulprit(r, in, l.StartIndex, l.EndIndex)
	}
}

func levelCulprit(r *result.Result, in Inputs, from, to int) *string {
	cut := *r
	cut.Capacity = nil
	cut.Runners = make([]result.Runner, len(r.Runners))
	for i, rn := range r.Runners {
		cut.Runners[i] = rn
		n := 0
		for n < len(rn.Buckets) && rn.Buckets[n].Index <= to {
			n++
		}
		cut.Runners[i].Buckets = rn.Buckets[:n:n]
	}
	a := Analyse(&cut, in)

	var best *result.Incident
	bestOverlap := 0
	for k := range a.Incidents {
		inc := &a.Incidents[k]
		overlap := min(inc.EndIndex, to) - max(inc.StartIndex, from) + 1
		if overlap > bestOverlap {
			best, bestOverlap = inc, overlap
		}
	}
	if best == nil {
		return nil
	}
	name := ""
	switch best.Class {
	case result.IncidentCorrelated:
		if best.Culprit != nil {
			name = best.Culprit.Runner
		}
	case result.IncidentStorageOnly:
		for _, ir := range best.Runners {
			if ir.Hot && kindOf(r, ir.Name) == "storage" {
				name = ir.Name
				break
			}
		}
	case result.IncidentAppOnly:
		for _, rn := range r.Runners {
			if rn.Kind == "app" {
				name = rn.Name
			}
		}
	}
	if name == "" {
		return nil
	}
	return &name
}

func kindOf(r *result.Result, name string) string {
	if rn := r.RunnerByName(name); rn != nil {
		return rn.Kind
	}
	return ""
}
