package metrics

import (
	"sort"
	"time"
)

// OpSummary is aggregate statistics over the measured window - the run excluding
// warm-up - for one runner or one label. The field names and units mirror
// schemas/result.schema.json.
type OpSummary struct {
	N           int64            `json:"n"`
	OK          int64            `json:"ok"`
	ErrorsTotal int64            `json:"errors_total"`
	ErrorRatio  float64          `json:"error_ratio"`
	Errors      map[string]int64 `json:"errors,omitempty"`

	Response   Quantiles `json:"response_ms"`
	Service    Quantiles `json:"service_ms"`
	ClientWait Quantiles `json:"client_wait_ms"`

	AchievedRPS float64 `json:"achieved_rps"`
	BytesIn     int64   `json:"bytes_in"`
	BytesOut    int64   `json:"bytes_out"`
}

// BucketSummary is one sealed bucket: what a fixed-size slice of the timeline looked
// like, after its sketches have been released.
type BucketSummary struct {
	Index        int     `json:"i"`
	OffsetMS     float64 `json:"t_ms"`
	N            int64   `json:"n"`
	Insufficient bool    `json:"insufficient,omitempty"`
	Warmup       bool    `json:"warmup,omitempty"`

	Response   Quantiles `json:"response_ms"`
	Service    Quantiles `json:"service_ms"`
	ClientWait Quantiles `json:"client_wait_ms"`

	Errors      map[string]int64 `json:"errors,omitempty"`
	ErrorsTotal int64            `json:"errors_total"`
	RPS         float64          `json:"rps"`

	Offered     int64 `json:"offered"`
	Dropped     int64 `json:"dropped"`
	InFlightMax int64 `json:"in_flight_max"`
	BytesIn     int64 `json:"bytes_in"`
}

// Snapshot is everything a collector has to say once a run has finished. It is the
// input to the result document, and therefore to every renderer.
type Snapshot struct {
	Runner string   `json:"runner"`
	Kind   Kind     `json:"kind"`
	Labels []string `json:"labels"`

	Summary        OpSummary            `json:"summary"`
	LabelSummaries map[string]OpSummary `json:"labels_summary"`

	Buckets      []BucketSummary            `json:"buckets"`
	LabelBuckets map[string][]BucketSummary `json:"label_buckets,omitempty"`

	Sketches        map[string]EncodedSketch `json:"sketches"`
	StatusHistogram map[string]int64         `json:"status_histogram,omitempty"`

	// Late counts outcomes that arrived after their bucket had already sealed. They
	// are included in the run totals; a non-zero value means the seal delay was
	// shorter than something actually took.
	Late int64 `json:"late"`
	// LabelOverflow reports that more labels were seen than MaxLabels, so the excess
	// was collected under "other".
	LabelOverflow bool `json:"label_overflow"`
	// LabelTimelineDropped reports that per-label timelines exceeded their retention
	// budget and were dropped. No analysis depends on them (ADR-002).
	LabelTimelineDropped bool `json:"label_timeline_dropped"`
	// Rejected counts observations that could not be recorded because a timing came
	// out NaN or infinite, and MergeFailures counts sketch merges that failed. Both
	// should be zero; they are reported rather than hidden so that a number which is
	// quietly wrong becomes a number that is visibly suspect.
	Rejected      int64 `json:"rejected"`
	MergeFailures int64 `json:"merge_failures"`

	// MeasuredWindow is the span the summary covers: from the end of warm-up to the
	// end of the last bucket that saw activity.
	MeasuredWindow time.Duration `json:"-"`
}

// Snapshot reduces everything collected so far into its final form. Call it after
// Finish.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	indexes := sortedBucketIndexes(c.buckets)
	labelBuckets := make(map[LabelID][]BucketSummary, len(c.labelBuckets))
	for k, v := range c.labelBuckets {
		labelBuckets[k] = v
	}
	dropped := c.labelTimelineDropped
	c.mu.Unlock()

	window := c.measuredWindow()

	snap := Snapshot{
		Runner:               c.cfg.Runner,
		Kind:                 c.cfg.Kind,
		Labels:               c.Labels(),
		Summary:              c.wholeRunner.summary(window),
		LabelSummaries:       map[string]OpSummary{},
		Sketches:             map[string]EncodedSketch{},
		Late:                 c.late.Load(),
		LabelOverflow:        c.overflowed.Load(),
		LabelTimelineDropped: dropped,
		Rejected:             c.wholeRunner.rejectedCount(),
		MergeFailures:        c.mergeFailures.Load(),
		MeasuredWindow:       window,
	}

	for id, a := range c.wholeLabels {
		name := c.labelNames[id]
		s := a.summary(window)
		if s.N == 0 {
			continue
		}
		snap.LabelSummaries[name] = s
		snap.Sketches[name] = EncodeSketch(a.response, c.cfg.RelativeAccuracy)
	}
	snap.Sketches[RunnerSketchKey] = EncodeSketch(c.wholeRunner.response, c.cfg.RelativeAccuracy)

	// The timeline is contiguous from the first to the last bucket that saw activity.
	// Gaps are emitted as empty buckets rather than omitted, because the analysis
	// reads "the previous 30 buckets" and a hole would silently shift that window.
	if len(indexes) > 0 {
		first, last := indexes[0], indexes[len(indexes)-1]
		snap.Buckets = make([]BucketSummary, 0, last-first+1)
		c.mu.Lock()
		for i := first; i <= last; i++ {
			if b, ok := c.buckets[i]; ok {
				snap.Buckets = append(snap.Buckets, *b)
				continue
			}
			snap.Buckets = append(snap.Buckets, BucketSummary{
				Index:        int(i),
				OffsetMS:     float64(time.Duration(i) * c.cfg.BucketWidth / time.Millisecond),
				Warmup:       i < c.warmupBuckets,
				Insufficient: true,
				Errors:       map[string]int64{},
			})
		}
		c.mu.Unlock()
	}

	if len(labelBuckets) > 0 {
		snap.LabelBuckets = make(map[string][]BucketSummary, len(labelBuckets))
		for id, bs := range labelBuckets {
			sort.Slice(bs, func(i, j int) bool { return bs[i].Index < bs[j].Index })
			snap.LabelBuckets[c.labelNames[id]] = bs
		}
	}

	if c.cfg.TrackStatuses {
		snap.StatusHistogram = map[string]int64{}
		for code := range c.statuses {
			if n := c.statuses[code].Load(); n > 0 {
				snap.StatusHistogram[itoa(code)] = n
			}
		}
		if n := c.statusesOther.Load(); n > 0 {
			snap.StatusHistogram["other"] = n
		}
		if len(snap.StatusHistogram) == 0 {
			snap.StatusHistogram = nil
		}
	}
	return snap
}

// measuredWindow is the span the whole-run summary covers: from the end of warm-up to
// the end of the last bucket that saw activity. Throughput is computed against it, so
// a run that idles at the end is not credited with a higher rate than it achieved.
func (c *Collector) measuredWindow() time.Duration {
	if !c.seenAny.Load() {
		return 0
	}
	last := c.maxBucket.Load()
	start := c.warmupBuckets
	if start > last+1 {
		return 0
	}
	return time.Duration(last+1-start) * c.cfg.BucketWidth
}

// rejectedCount reads the rejection tally under the accumulator's own lock.
func (a *accum) rejectedCount() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rejected
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
