package analysis

import (
	"time"

	"github.com/IshaanNene/Tracepoint/internal/result"
)

// Hot-bucket reasons.
const (
	ReasonAbsolute = "absolute"
	ReasonRelative = "relative"
	ReasonBoth     = "both"
)

// thresholds resolves each runner's absolute hot-bucket threshold: the SLO p99 if one
// is configured, else the --<runner>-threshold flag, else the default (§5.5).
func thresholds(runners []string, in Inputs) map[string]result.Threshold {
	p := in.params()
	out := make(map[string]result.Threshold, len(runners))
	for _, name := range runners {
		if t := in.SLO.For(name); t.P99 != nil {
			out[name] = result.Threshold{ThresholdMS: msOf(t.P99.D()), Source: SourceSLO}
			continue
		}
		if v, ok := in.FlagThresholds[name]; ok && v > 0 {
			out[name] = result.Threshold{ThresholdMS: v, Source: SourceFlag}
			continue
		}
		out[name] = result.Threshold{ThresholdMS: p.DefaultThresholdMS, Source: SourceDefault}
	}
	return out
}

// eligible reports whether a bucket may be used as evidence at all: outside warm-up
// and with enough samples for its p99 to mean something.
func eligible(b *result.Bucket) bool {
	return !b.Warmup && !b.Insufficient && b.N > 0
}

// hotBuckets applies both rules of ADR-005 to one runner's service-time p99 series.
//
// The absolute rule compares against the threshold. The relative rule compares against
// the median of the previous Window steady buckets - eligible buckets that were not
// themselves hot, so an ongoing slowdown never becomes its own baseline - and fires
// only when the modified z-score, the ratio and the absolute difference all clear
// their bars.
func hotBuckets(buckets []result.Bucket, thresholdMS float64, p Params) []result.HotBucket {
	var (
		out    []result.HotBucket
		steady []float64 // service p99 of recent steady buckets, oldest first
	)
	for i := range buckets {
		b := &buckets[i]
		if !eligible(b) {
			continue
		}
		x := b.Service.P99
		absolute := x > thresholdMS

		relative, base, mz := false, 0.0, 0.0
		if len(steady) >= p.MinBaseline {
			window := steady
			if len(window) > p.Window {
				window = window[len(window)-p.Window:]
			}
			base = median(window)
			dev := mad(window, base)
			// A MAD below the sketch's own resolution is measurement noise, and a MAD
			// of zero would make every flicker an infinite outlier.
			if floor := base * p.Resolution; dev < floor {
				dev = floor
			}
			if dev < 1e-3 {
				dev = 1e-3
			}
			mz = 0.6745 * (x - base) / dev
			relative = mz > p.MZThreshold && x >= p.RatioGuard*base && x-base >= p.AbsGuardMS
		}

		if !absolute && !relative {
			steady = append(steady, x)
			continue
		}
		hb := result.HotBucket{Index: b.Index, P99MS: round3(x)}
		switch {
		case absolute && relative:
			hb.Reason = ReasonBoth
		case absolute:
			hb.Reason = ReasonAbsolute
		default:
			hb.Reason = ReasonRelative
		}
		if base > 0 {
			hb.BaselineMS = round3(base)
			hb.MZScore = round2(mz)
		}
		out = append(out, hb)
	}
	return out
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
