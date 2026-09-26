package analysis

import (
	"github.com/IshaanNene/Tracepoint/internal/config"
)

// Params are the constants of the method. Every one of them is argued for in
// docs/adr/005-correlation-methodology.md and docs/METHODOLOGY.md; they live in one
// place so that a reader can audit them and a test can vary them.
type Params struct {
	// DefaultThresholdMS is the absolute hot-bucket threshold when neither an SLO nor
	// a flag sets one.
	DefaultThresholdMS float64
	// Window is how many previous steady buckets form the rolling baseline.
	Window int
	// MinBaseline is how many steady buckets the baseline needs before the relative
	// rule may fire at all.
	MinBaseline int
	// MZThreshold is the modified z-score a bucket must exceed.
	MZThreshold float64
	// RatioGuard and AbsGuardMS are the two extra conditions of the relative rule:
	// the bucket must be at least this many times the baseline and this many
	// milliseconds above it.
	RatioGuard float64
	AbsGuardMS float64
	// Resolution is the sketches' relative accuracy. Differences smaller than this
	// are below measurement resolution, so the MAD is floored at it.
	Resolution float64
	// MergeGap is the largest gap, in buckets, bridged when merging one runner's hot
	// buckets into an episode.
	MergeGap int
	// Tolerance is how many buckets either side two runners' episodes may be apart
	// and still count as the same incident.
	Tolerance int
	// ClientDominance is the share of response time spent waiting inside the
	// generator above which a hot episode is attributed to the generator.
	ClientDominance float64
	// MaxLag is the largest lag, in buckets, the whole-run correlation tries.
	MaxLag int
	// MinPairs is how many bucket pairs a correlation needs to be reported as
	// leaning anywhere.
	MinPairs int
	// LeanRho is the |rho| below which a correlation leans nowhere.
	LeanRho float64
	// StrongRho is the |rho| that counts as consistent evidence for confidence.
	StrongRho float64
	// TieEpsilon is how close two culprit scores must be to be reported as a tie.
	TieEpsilon float64
	// StallRatio and StallAbsMS are how far every runner's client wait must jump
	// above its own median, together, for a bucket to count as a generator stall;
	// StallMinRunners is how many runners it takes to tell a stall from a queue, and
	// StallMaxBuckets the longest run of buckets a pause can span.
	StallRatio      float64
	StallAbsMS      float64
	StallMinRunners int
	StallMaxBuckets int
	// StallServiceFactor is how many times the pause a tier's service-time rise must
	// exceed, over its threshold, to be the target moving rather than the pause.
	StallServiceFactor float64
	// StrainFactor, StrainRun and StrainEarly* define the strain finder (§5.5.8): p99
	// above StrainFactor times the median of the early buckets - the first
	// StrainEarlyShare of them, clamped to [StrainEarlyMin, StrainEarlyMax] - for
	// StrainRun consecutive buckets.
	StrainFactor     float64
	StrainRun        int
	StrainEarlyShare float64
	StrainEarlyMin   int
	StrainEarlyMax   int
}

// DefaultParams are the values the specification fixes (§5.5).
func DefaultParams() Params {
	return Params{
		DefaultThresholdMS: 100,
		Window:             30,
		MinBaseline:        10,
		MZThreshold:        3.5,
		RatioGuard:         3,
		AbsGuardMS:         5,
		Resolution:         0.01,
		MergeGap:           1,
		Tolerance:          1,
		ClientDominance:    0.5,
		MaxLag:             3,
		MinPairs:           10,
		LeanRho:            0.3,
		StrongRho:          0.7,
		TieEpsilon:         0.25,
		StallRatio:         3,
		StallAbsMS:         5,
		StallMinRunners:    2,
		StallMaxBuckets:    2,
		StallServiceFactor: 10,
		StrainFactor:       2,
		StrainRun:          3,
		StrainEarlyShare:   0.1,
		StrainEarlyMin:     5,
		StrainEarlyMax:     30,
	}
}

// Inputs is what the analysis needs beyond the recorded data itself.
type Inputs struct {
	// SLO holds the budgets: judged on response time, and the source of each
	// runner's absolute hot-bucket threshold.
	SLO config.SLO
	// FlagThresholds are --<runner>-threshold values in milliseconds, used when a
	// runner has no SLO p99.
	FlagThresholds map[string]float64
	// Telemetry names the samplers the configuration asked for, so one that failed
	// to deliver can be reported.
	Telemetry []string
	// Params overrides the method's constants. The zero value means DefaultParams.
	Params *Params
}

func (in Inputs) params() Params {
	if in.Params != nil {
		return *in.Params
	}
	return DefaultParams()
}

// Threshold sources.
const (
	SourceSLO     = "slo"
	SourceFlag    = "flag"
	SourceDefault = "default"
)
