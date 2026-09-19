package metrics

import (
	"encoding/base64"
	"fmt"
	"math"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/DataDog/sketches-go/ddsketch/mapping"
	"github.com/DataDog/sketches-go/ddsketch/store"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// DefaultRelativeAccuracy is the sketch guarantee: any quantile read back is within
// 1% of the true value. Relative rather than absolute accuracy is the right shape for
// latency, where 1% of a two-second p99 matters and 1% of a 200-microsecond p50 does
// not (docs/adr/002-sketches-and-memory.md).
const DefaultRelativeAccuracy = 0.01

// SketchEncoding names the wire format written into result.json. It is the library's
// own compact binary form, base64-encoded - not protobuf, which would pull in a
// dependency for no benefit.
const SketchEncoding = "ddsketch-bin-base64"

// RunnerSketchKey is the key under which a runner's whole-run sketch is stored,
// alongside one entry per label. The double underscores keep it from colliding with a
// label name.
const RunnerSketchKey = "__runner__"

// EncodedSketch is a serialised whole-run sketch, kept in result.json so `compare`
// can compute order-statistic confidence intervals on quantiles without needing the
// raw observations (spec §5.7).
type EncodedSketch struct {
	Encoding         string  `json:"encoding"`
	RelativeAccuracy float64 `json:"relative_accuracy"`
	Count            float64 `json:"count"`
	Data             string  `json:"data"`
}

// newSketch builds an empty sketch at the given relative accuracy.
func newSketch(relativeAccuracy float64) (*ddsketch.DDSketch, error) {
	m, err := mapping.NewLogarithmicMapping(relativeAccuracy)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "building a sketch at %v relative accuracy", relativeAccuracy)
	}
	return ddsketch.NewDDSketch(m, store.NewDenseStore(), store.NewDenseStore()), nil
}

// EncodeSketch serialises a sketch for result.json.
func EncodeSketch(s *ddsketch.DDSketch, relativeAccuracy float64) EncodedSketch {
	var buf []byte
	s.Encode(&buf, false)
	return EncodedSketch{
		Encoding:         SketchEncoding,
		RelativeAccuracy: relativeAccuracy,
		Count:            s.GetCount(),
		Data:             base64.StdEncoding.EncodeToString(buf),
	}
}

// DecodeSketch reverses EncodeSketch. The round trip is exact: quantiles read from
// the decoded sketch equal those read from the original.
func DecodeSketch(e EncodedSketch) (*ddsketch.DDSketch, error) {
	if e.Encoding != SketchEncoding {
		return nil, errs.New(errs.CodeResultParse, "unknown sketch encoding %q", e.Encoding).
			WithHint("this result was written by a newer TracePoint; upgrade to read it")
	}
	raw, err := base64.StdEncoding.DecodeString(e.Data)
	if err != nil {
		return nil, errs.Wrap(errs.CodeResultParse, err, "decoding a sketch")
	}
	accuracy := e.RelativeAccuracy
	if accuracy <= 0 {
		accuracy = DefaultRelativeAccuracy
	}
	m, err := mapping.NewLogarithmicMapping(accuracy)
	if err != nil {
		return nil, errs.Wrap(errs.CodeResultParse, err, "rebuilding the sketch mapping")
	}
	s, err := ddsketch.DecodeDDSketch(raw, store.DenseStoreConstructor, m)
	if err != nil {
		return nil, errs.Wrap(errs.CodeResultParse, err, "decoding a sketch")
	}
	return s, nil
}

// Quantiles is a latency distribution in milliseconds.
//
// P50 through P999 are read from the sketch and carry its relative-error guarantee.
// Max and Mean are tracked exactly alongside it, because both are cheap to keep and
// a reader comparing an exact maximum against an approximate p99.9 learns something
// a sketched maximum would hide.
type Quantiles struct {
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p999"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// quantilesFrom reads the standard set out of a sketch. count, sum and max are passed
// in because they are tracked exactly rather than derived from the sketch.
func quantilesFrom(s *ddsketch.DDSketch, count int64, sum, maxValue float64) Quantiles {
	q := Quantiles{Max: maxValue}
	if count > 0 {
		q.Mean = sum / float64(count)
	}
	if s == nil || s.GetCount() == 0 {
		return q
	}
	read := func(p float64) float64 {
		v, err := s.GetValueAtQuantile(p)
		if err != nil || math.IsNaN(v) {
			return 0
		}
		if v < 0 {
			return 0
		}
		return v
	}
	q.P50, q.P90, q.P95, q.P99, q.P999 = read(0.50), read(0.90), read(0.95), read(0.99), read(0.999)
	return q
}

// merge folds src into dst, allocating dst if it is empty.
func mergeSketch(dst, src *ddsketch.DDSketch) error {
	if src == nil || src.GetCount() == 0 {
		return nil
	}
	if err := dst.MergeWith(src); err != nil {
		return fmt.Errorf("merging sketches: %w", err)
	}
	return nil
}
