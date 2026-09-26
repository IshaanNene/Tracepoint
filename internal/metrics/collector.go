package metrics

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// MaxLabels caps how many labels one runner may report separately. Beyond it, further
// labels are collected under OverflowLabel rather than dropped, so totals stay correct
// while memory stays bounded - a caller who accidentally puts an id in a request name
// degrades the report instead of exhausting the machine.
const MaxLabels = 50

// OverflowLabel collects every label past MaxLabels.
const OverflowLabel = "other"

// DefaultLabelTimelineBudget caps how many per-label bucket summaries are retained.
// Per-runner timelines are always kept because every analysis reads them; per-label
// timelines exist for the report, so past this budget they are dropped and a finding
// records it (docs/adr/002-sketches-and-memory.md).
const DefaultLabelTimelineBudget = 20_000

// Kind says whether a runner drives the tier users talk to or probes one behind it.
type Kind string

// Runner kinds.
const (
	KindApp     Kind = "app"
	KindStorage Kind = "storage"
)

// Config configures a collector. It is fixed for the life of a run.
type Config struct {
	Runner string
	Kind   Kind
	// Labels are the request, step, query or command names this runner will report.
	// Resolving them up front is what keeps the record path from hashing strings.
	Labels []string

	BucketWidth time.Duration
	Warmup      time.Duration
	// SealDelay is how long past a bucket's end to wait before sealing it: the longest
	// operation timeout, after which nothing in flight can still land in that bucket.
	SealDelay  time.Duration
	MinSamples int

	RelativeAccuracy    float64
	LabelTimelineBudget int
	// TrackStatuses turns on the HTTP status histogram.
	TrackStatuses bool
}

func (c *Config) withDefaults() {
	if c.RelativeAccuracy <= 0 {
		c.RelativeAccuracy = DefaultRelativeAccuracy
	}
	if c.MinSamples <= 0 {
		c.MinSamples = 20
	}
	if c.LabelTimelineBudget == 0 {
		c.LabelTimelineBudget = DefaultLabelTimelineBudget
	}
	if c.Kind == "" {
		c.Kind = KindApp
	}
}

func (c *Config) validate() error {
	if c.Runner == "" {
		return errs.New(errs.CodeInternal, "a collector needs a runner name")
	}
	if c.BucketWidth <= 0 {
		return errs.New(errs.CodeConfigInvalidValue, "bucket width must be positive, got %v", c.BucketWidth).
			WithPath("/run/bucket")
	}
	if c.Warmup < 0 {
		return errs.New(errs.CodeConfigInvalidValue, "warm-up must not be negative, got %v", c.Warmup).
			WithPath("/run/warmup")
	}
	if c.SealDelay < 0 {
		return errs.New(errs.CodeConfigInvalidValue, "the seal delay must not be negative, got %v", c.SealDelay).
			WithPath("/run/timeout")
	}
	return nil
}

// accum accumulates one label's observations, either for an open bucket or for the
// whole run. Counters and sketches share one mutex: the critical section is a single
// sketch Add, and holding one lock rather than three keeps the record path short.
type accum struct {
	mu sync.Mutex

	n        int64
	errs     [numClasses]int64
	bytesIn  int64
	bytesOut int64

	response, service, clientWait *ddsketch.DDSketch

	sumResp, sumSvc, sumWait float64
	maxResp, maxSvc, maxWait float64

	// rejected counts observations a logarithmic sketch cannot represent - a timing
	// that came out NaN or infinite. It should always be zero; a non-zero value
	// reaches the snapshot so a wrong number is visible rather than silent.
	rejected int64
}

func newAccum(relativeAccuracy float64) (*accum, error) {
	a := &accum{}
	var err error
	if a.response, err = newSketch(relativeAccuracy); err != nil {
		return nil, err
	}
	if a.service, err = newSketch(relativeAccuracy); err != nil {
		return nil, err
	}
	if a.clientWait, err = newSketch(relativeAccuracy); err != nil {
		return nil, err
	}
	return a, nil
}

// add records one outcome. Callers hold no other lock.
func (a *accum) add(o *Outcome) {
	resp := ms(o.ResponseTime())
	svc := ms(o.ServiceTime())
	wait := ms(o.ClientWait())

	a.mu.Lock()
	a.n++
	a.errs[o.Class]++
	a.bytesIn += o.BytesIn
	a.bytesOut += o.BytesOut

	// Negative values are impossible for a well-formed outcome but would poison a
	// logarithmic sketch, so they are clamped rather than trusted.
	ok := addPositive(a.response, resp)
	ok = addPositive(a.service, svc) && ok
	ok = addPositive(a.clientWait, wait) && ok
	if !ok {
		a.rejected++
	}

	a.sumResp += resp
	a.sumSvc += svc
	a.sumWait += wait
	a.maxResp = maxOf(a.maxResp, resp)
	a.maxSvc = maxOf(a.maxSvc, svc)
	a.maxWait = maxOf(a.maxWait, wait)
	a.mu.Unlock()
}

// mergeInto folds this accumulator into dst, leaving this one intact. The caller holds
// the owning slot's write lock, so no concurrent add on the source can be in progress;
// dst takes its own lock because the whole-run accumulators are also read by Snapshot.
func (a *accum) mergeInto(dst *accum) error {
	if dst != nil && a.n > 0 {
		dst.mu.Lock()
		dst.n += a.n
		for i, v := range a.errs {
			dst.errs[i] += v
		}
		dst.bytesIn += a.bytesIn
		dst.bytesOut += a.bytesOut
		dst.sumResp += a.sumResp
		dst.sumSvc += a.sumSvc
		dst.sumWait += a.sumWait
		dst.maxResp = maxOf(dst.maxResp, a.maxResp)
		dst.maxSvc = maxOf(dst.maxSvc, a.maxSvc)
		dst.maxWait = maxOf(dst.maxWait, a.maxWait)
		dst.rejected += a.rejected
		err := firstErr(
			mergeSketch(dst.response, a.response),
			mergeSketch(dst.service, a.service),
			mergeSketch(dst.clientWait, a.clientWait),
		)
		dst.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *accum) reset() {
	a.n = 0
	a.errs = [numClasses]int64{}
	a.bytesIn, a.bytesOut = 0, 0
	a.sumResp, a.sumSvc, a.sumWait = 0, 0, 0
	a.maxResp, a.maxSvc, a.maxWait = 0, 0, 0
	a.rejected = 0
	a.response.Clear()
	a.service.Clear()
	a.clientWait.Clear()
}

func (a *accum) summary(window time.Duration) OpSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := OpSummary{
		N:          a.n,
		OK:         a.errs[ClassOK],
		Response:   quantilesFrom(a.response, a.n, a.sumResp, a.maxResp),
		Service:    quantilesFrom(a.service, a.n, a.sumSvc, a.maxSvc),
		ClientWait: quantilesFrom(a.clientWait, a.n, a.sumWait, a.maxWait),
		BytesIn:    a.bytesIn,
		BytesOut:   a.bytesOut,
		Errors:     map[string]int64{},
	}
	s.ErrorsTotal = s.N - s.OK
	if s.N > 0 {
		s.ErrorRatio = float64(s.ErrorsTotal) / float64(s.N)
	}
	for i, v := range a.errs {
		if v > 0 && Class(i) != ClassOK {
			s.Errors[Class(i).String()] = v
		}
	}
	if window > 0 {
		s.AchievedRPS = float64(s.N) / window.Seconds()
	}
	return s
}

// slot is one open bucket. The read-write mutex is the transition guard: recorders
// take it shared, and only sealing - which resets the slot - takes it exclusively.
// Within a shared hold, each label's accumulator has its own mutex, so traffic spread
// across labels does not contend.
type slot struct {
	mu     sync.RWMutex
	index  int64 // -1 when free
	labels []*accum

	offered     atomic.Int64
	dropped     atomic.Int64
	inFlightMax atomic.Int64
}

// Collector records one runner's outcomes and reduces them to summaries.
//
// It is safe for concurrent use by every worker of a runner.
type Collector struct {
	cfg Config

	labelNames []string
	labelIndex map[string]LabelID
	overflowID LabelID
	overflowed atomic.Bool

	warmupBuckets int64
	ring          []*slot

	// statuses is a flat table rather than a map: lock-free, allocation-free, and
	// 4.8 KB for the whole run.
	statuses      [600]atomic.Int64
	statusesOther atomic.Int64

	late          atomic.Int64
	mergeFailures atomic.Int64
	minBucket     atomic.Int64
	maxBucket     atomic.Int64
	seenAny       atomic.Bool
	sealedAbove   atomic.Int64 // highest bucket index already sealed, -1 when none

	// whole-run accumulators, post-warm-up only. Guarded by their own mutexes.
	wholeRunner *accum
	wholeLabels []*accum

	// sealed output
	mu                   sync.Mutex
	buckets              map[int64]*BucketSummary
	labelBuckets         map[LabelID][]BucketSummary
	labelTimelineEntries int
	labelTimelineDropped bool
	// windows are spans of buckets merged into one accumulator as they seal, for a
	// capacity level's steady window. Guarded by mu.
	windows []*window
}

// window is a half-open span of bucket indexes, [from, to), and what its buckets held.
type window struct {
	from, to         int64
	acc              *accum
	offered, dropped int64
}

// WindowSummary is a window's figures, from sketches merged across its buckets -
// never an average of their percentiles.
type WindowSummary struct {
	OpSummary
	Offered int64 `json:"offered"`
	Dropped int64 `json:"dropped"`
}

// NewCollector builds a collector for one runner.
func NewCollector(cfg Config) (*Collector, error) {
	cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	c := &Collector{
		cfg:          cfg,
		labelIndex:   make(map[string]LabelID, len(cfg.Labels)+1),
		buckets:      map[int64]*BucketSummary{},
		labelBuckets: map[LabelID][]BucketSummary{},
	}
	c.sealedAbove.Store(-1)

	for _, name := range cfg.Labels {
		if len(c.labelNames) >= MaxLabels {
			c.overflowed.Store(true)
			break
		}
		if _, seen := c.labelIndex[name]; seen {
			continue
		}
		c.labelIndex[name] = LabelID(len(c.labelNames))
		c.labelNames = append(c.labelNames, name)
	}
	// The overflow label always exists, so LabelID can answer for an unknown name
	// without allocating or failing.
	c.overflowID = LabelID(len(c.labelNames))
	c.labelNames = append(c.labelNames, OverflowLabel)
	c.labelIndex[OverflowLabel] = c.overflowID

	// A bucket is treated as warm-up when any part of it falls inside the warm-up
	// window. Rounding up rather than down means warm-up can never contaminate a
	// steady-state baseline, which is the direction that matters.
	if cfg.Warmup > 0 {
		c.warmupBuckets = int64((cfg.Warmup + cfg.BucketWidth - 1) / cfg.BucketWidth)
	}

	var err error
	if c.wholeRunner, err = newAccum(cfg.RelativeAccuracy); err != nil {
		return nil, err
	}
	c.wholeLabels = make([]*accum, len(c.labelNames))
	for i := range c.wholeLabels {
		if c.wholeLabels[i], err = newAccum(cfg.RelativeAccuracy); err != nil {
			return nil, err
		}
	}

	// The ring holds every bucket that could still receive an outcome, plus slack so
	// a late seal does not force a bucket out before its time.
	open := int(cfg.SealDelay/cfg.BucketWidth) + 2
	ringSize := open + 2
	if ringSize < 4 {
		ringSize = 4
	}
	c.ring = make([]*slot, ringSize)
	for i := range c.ring {
		s := &slot{index: -1, labels: make([]*accum, len(c.labelNames))}
		for j := range s.labels {
			if s.labels[j], err = newAccum(cfg.RelativeAccuracy); err != nil {
				return nil, err
			}
		}
		c.ring[i] = s
	}
	return c, nil
}

// LabelID resolves a label name to its identifier, once, before the run starts.
// Unknown names - and everything past MaxLabels - resolve to the overflow label.
func (c *Collector) LabelID(name string) LabelID {
	if id, ok := c.labelIndex[name]; ok {
		return id
	}
	c.overflowed.Store(true)
	return c.overflowID
}

// Labels returns the label names in identifier order. The overflow label is reported
// only when something actually overflowed into it, so an ordinary run lists exactly
// the labels it was configured with.
func (c *Collector) Labels() []string {
	out := append([]string(nil), c.labelNames...)
	if !c.overflowed.Load() && len(out) > 0 && out[len(out)-1] == OverflowLabel {
		out = out[:len(out)-1]
	}
	return out
}

// BucketIndexOf returns the bucket an offset falls in.
func (c *Collector) BucketIndexOf(at time.Duration) int64 {
	if at < 0 {
		return 0
	}
	return int64(at / c.cfg.BucketWidth)
}

// Record files one completed operation. It does not retain o, so a caller may reuse
// the same struct for every operation.
func (c *Collector) Record(o *Outcome) {
	if o.Label < 0 || int(o.Label) >= len(c.labelNames) {
		o.Label = c.overflowID
	}
	if c.cfg.TrackStatuses && o.Status != 0 {
		if o.Status > 0 && int(o.Status) < len(c.statuses) {
			c.statuses[o.Status].Add(1)
		} else {
			c.statusesOther.Add(1)
		}
	}

	idx := c.BucketIndexOf(o.Intended)
	// A bucket that has already sealed cannot be reopened - its summary is written and
	// its sketches are freed. The outcome still happened, so it is counted towards the
	// run totals rather than lost, and the late tally records that the seal delay was
	// shorter than something actually took.
	if idx <= c.sealedAbove.Load() {
		c.noteBucket(idx)
		c.recordLate(o, idx)
		return
	}
	c.noteBucket(idx)

	s := c.ring[idx%int64(len(c.ring))]
	s.mu.RLock()
	if s.index == idx {
		s.labels[o.Label].add(o)
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	// The slot is free, stale, or holds a newer bucket. Take it exclusively to decide.
	s.mu.Lock()
	switch {
	case s.index == idx:
		// Another goroutine claimed it for us in the meantime.
	case s.index < 0 || s.index < idx:
		// Free, or holding an older bucket that should already have been sealed. Seal
		// it now rather than discarding it, then claim the slot.
		if s.index >= 0 {
			c.sealSlotLocked(s)
		}
		s.index = idx
	default:
		// The slot holds a newer bucket, so this outcome's bucket is already gone. It
		// still happened: count it towards the run rather than losing it.
		s.mu.Unlock()
		c.recordLate(o, idx)
		return
	}
	s.labels[o.Label].add(o)
	s.mu.Unlock()
}

// recordLate books an outcome whose bucket has already sealed.
func (c *Collector) recordLate(o *Outcome, idx int64) {
	c.late.Add(1)
	if idx < c.warmupBuckets {
		return
	}
	c.wholeLabels[o.Label].add(o)
	c.wholeRunner.add(o)
}

// RecordOffered counts an arrival the schedule called for, whether or not it ran.
func (c *Collector) RecordOffered(bucketIndex int64) {
	c.counterSlot(bucketIndex, func(s *slot) { s.offered.Add(1) })
}

// RecordDropped counts an arrival refused because every worker was busy and the
// dispatch queue was full. Drops are data, not errors: read with flat service time
// they mean the generator was capped, and with rising service time they mean the
// target was saturating (spec §5.5).
func (c *Collector) RecordDropped(bucketIndex int64) {
	c.counterSlot(bucketIndex, func(s *slot) { s.dropped.Add(1) })
}

// ObserveInFlight records a concurrency reading; the bucket keeps the peak.
func (c *Collector) ObserveInFlight(bucketIndex, n int64) {
	c.counterSlot(bucketIndex, func(s *slot) {
		for {
			cur := s.inFlightMax.Load()
			if n <= cur || s.inFlightMax.CompareAndSwap(cur, n) {
				return
			}
		}
	})
}

// counterSlot applies an executor counter to a bucket, claiming or sealing the ring
// slot as needed so that a bucket with no completed operations still appears on the
// timeline.
func (c *Collector) counterSlot(idx int64, apply func(*slot)) {
	c.noteBucket(idx)
	if idx <= c.sealedAbove.Load() {
		return // the bucket is closed; its offered and dropped counts are already written
	}
	s := c.ring[idx%int64(len(c.ring))]

	s.mu.RLock()
	if s.index == idx {
		apply(s)
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	s.mu.Lock()
	switch {
	case s.index == idx:
	case s.index < 0 || s.index < idx:
		if s.index >= 0 {
			c.sealSlotLocked(s)
		}
		s.index = idx
	default:
		s.mu.Unlock()
		return
	}
	apply(s)
	s.mu.Unlock()
}

func (c *Collector) noteBucket(idx int64) {
	if !c.seenAny.Swap(true) {
		c.minBucket.Store(idx)
		c.maxBucket.Store(idx)
		return
	}
	for {
		cur := c.minBucket.Load()
		if idx >= cur || c.minBucket.CompareAndSwap(cur, idx) {
			break
		}
	}
	for {
		cur := c.maxBucket.Load()
		if idx <= cur || c.maxBucket.CompareAndSwap(cur, idx) {
			break
		}
	}
}

// SealThrough seals every bucket that can no longer receive an outcome: those whose
// end, plus the longest operation timeout, is at or before now.
//
// Sealing is what keeps memory flat. Until a bucket seals it holds live sketches;
// afterwards it is a fixed-size summary and the sketches are released.
func (c *Collector) SealThrough(now time.Duration) {
	for _, s := range c.ring {
		s.mu.RLock()
		idx := s.index
		s.mu.RUnlock()
		if idx < 0 {
			continue
		}
		if c.sealDeadline(idx) > now {
			continue
		}
		s.mu.Lock()
		if s.index >= 0 && c.sealDeadline(s.index) <= now {
			c.sealSlotLocked(s)
		}
		s.mu.Unlock()
	}
}

// merge folds src into dst, counting the failure rather than discarding it. A merge
// can only fail if two sketches disagree about their index mapping, which construction
// makes impossible - but losing a bucket's worth of observations silently is exactly
// the kind of thing that turns into an inexplicable percentile later.
func (c *Collector) merge(dst, src *accum) {
	if err := src.mergeInto(dst); err != nil {
		c.mergeFailures.Add(1)
	}
}

func (c *Collector) sealDeadline(idx int64) time.Duration {
	return time.Duration(idx+1)*c.cfg.BucketWidth + c.cfg.SealDelay
}

// SealedAfter returns copies of the sealed buckets with an index above after, in index
// order - what a live event stream reports as each bucket closes. Buckets seal in
// index order, so a caller that remembers the last index it saw never misses one.
func (c *Collector) SealedAfter(after int64) []BucketSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	var idx []int64
	for i := range c.buckets {
		if i > after {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	out := make([]BucketSummary, 0, len(idx))
	for _, i := range idx {
		out = append(out, *c.buckets[i])
	}
	return out
}

// OpenBuckets reports how many buckets still hold live sketches.
func (c *Collector) OpenBuckets() int {
	n := 0
	for _, s := range c.ring {
		s.mu.RLock()
		if s.index >= 0 {
			n++
		}
		s.mu.RUnlock()
	}
	return n
}

// sealSlotLocked reduces a slot to summaries and frees it. The caller holds s.mu
// exclusively.
func (c *Collector) sealSlotLocked(s *slot) {
	idx := s.index
	warmup := idx < c.warmupBuckets

	summary := BucketSummary{
		Index:       int(idx),
		OffsetMS:    float64(time.Duration(idx) * c.cfg.BucketWidth / time.Millisecond),
		Warmup:      warmup,
		Offered:     s.offered.Load(),
		Dropped:     s.dropped.Load(),
		InFlightMax: s.inFlightMax.Load(),
	}

	runnerRoll, err := newAccum(c.cfg.RelativeAccuracy)
	if err != nil {
		// Only reachable if the sketch mapping is invalid, which construction already
		// rejected. Drop the bucket's detail rather than panicking in a load test.
		runnerRoll = nil
	}

	for id, a := range s.labels {
		if a.n == 0 {
			continue
		}
		// Per-label timeline first, while the accumulator still holds this bucket.
		c.recordLabelBucket(LabelID(id), idx, a, warmup)
		if runnerRoll != nil {
			c.merge(runnerRoll, a)
		}
		// Warm-up is excluded from every whole-run figure, but its buckets still
		// appear on the timeline, shaded rather than hidden.
		if !warmup {
			c.merge(c.wholeLabels[id], a)
		}
		a.reset()
	}

	c.mu.Lock()
	for _, w := range c.windows {
		if idx >= w.from && idx < w.to {
			w.offered += summary.Offered
			w.dropped += summary.Dropped
			if runnerRoll != nil {
				c.merge(w.acc, runnerRoll)
			}
		}
	}
	c.mu.Unlock()

	if runnerRoll != nil {
		summary.N = runnerRoll.n
		summary.ErrorsTotal = runnerRoll.n - runnerRoll.errs[ClassOK]
		summary.Errors = map[string]int64{}
		for i, v := range runnerRoll.errs {
			if v > 0 && Class(i) != ClassOK {
				summary.Errors[Class(i).String()] = v
			}
		}
		summary.Response = quantilesFrom(runnerRoll.response, runnerRoll.n, runnerRoll.sumResp, runnerRoll.maxResp)
		summary.Service = quantilesFrom(runnerRoll.service, runnerRoll.n, runnerRoll.sumSvc, runnerRoll.maxSvc)
		summary.ClientWait = quantilesFrom(runnerRoll.clientWait, runnerRoll.n, runnerRoll.sumWait, runnerRoll.maxWait)
		summary.BytesIn = runnerRoll.bytesIn
		if secs := c.cfg.BucketWidth.Seconds(); secs > 0 {
			summary.RPS = float64(runnerRoll.n) / secs
		}
		if !warmup {
			c.merge(c.wholeRunner, runnerRoll)
		}
	}
	summary.Insufficient = summary.N < int64(c.cfg.MinSamples)

	c.mu.Lock()
	c.buckets[idx] = &summary
	c.mu.Unlock()

	s.offered.Store(0)
	s.dropped.Store(0)
	s.inFlightMax.Store(0)
	s.index = -1
	for {
		cur := c.sealedAbove.Load()
		if idx <= cur || c.sealedAbove.CompareAndSwap(cur, idx) {
			break
		}
	}
}

// recordLabelBucket stores a per-label bucket summary, subject to the retention
// budget from ADR-002.
func (c *Collector) recordLabelBucket(id LabelID, idx int64, a *accum, warmup bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.labelTimelineDropped {
		return
	}
	if c.labelTimelineEntries >= c.cfg.LabelTimelineBudget {
		c.labelTimelineDropped = true
		c.labelBuckets = map[LabelID][]BucketSummary{}
		return
	}
	c.labelTimelineEntries++

	b := BucketSummary{
		Index:        int(idx),
		OffsetMS:     float64(time.Duration(idx) * c.cfg.BucketWidth / time.Millisecond),
		N:            a.n,
		Warmup:       warmup,
		Insufficient: a.n < int64(c.cfg.MinSamples),
		ErrorsTotal:  a.n - a.errs[ClassOK],
		Response:     quantilesFrom(a.response, a.n, a.sumResp, a.maxResp),
		Service:      quantilesFrom(a.service, a.n, a.sumSvc, a.maxSvc),
		ClientWait:   quantilesFrom(a.clientWait, a.n, a.sumWait, a.maxWait),
		BytesIn:      a.bytesIn,
	}
	b.Errors = map[string]int64{}
	for i, v := range a.errs {
		if v > 0 && Class(i) != ClassOK {
			b.Errors[Class(i).String()] = v
		}
	}
	if secs := c.cfg.BucketWidth.Seconds(); secs > 0 {
		b.RPS = float64(b.N) / secs
	}
	c.labelBuckets[id] = append(c.labelBuckets[id], b)
}

// OpenWindow starts collecting the buckets [from, to) into one summary, and returns
// its identifier. Open it before the first of its buckets seals.
func (c *Collector) OpenWindow(from, to int64) (int, error) {
	acc, err := newAccum(c.cfg.RelativeAccuracy)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windows = append(c.windows, &window{from: from, to: to, acc: acc})
	return len(c.windows) - 1, nil
}

// Window summarises a window's sealed buckets. Throughput is over the window's whole
// span.
func (c *Collector) Window(id int) WindowSummary {
	c.mu.Lock()
	w := c.windows[id]
	offered, dropped := w.offered, w.dropped
	c.mu.Unlock()
	return WindowSummary{
		OpSummary: w.acc.summary(time.Duration(w.to-w.from) * c.cfg.BucketWidth),
		Offered:   offered, Dropped: dropped,
	}
}

// SealBefore seals every open bucket with an index below idx, without waiting for the
// seal delay. Call it only when nothing still in flight can belong to those buckets -
// between the levels of a capacity search, once a level has drained.
func (c *Collector) SealBefore(idx int64) {
	// In index order, as sealing always is: the event stream relies on it.
	type open struct {
		s   *slot
		idx int64
	}
	var slots []open
	for _, s := range c.ring {
		s.mu.RLock()
		if s.index >= 0 && s.index < idx {
			slots = append(slots, open{s, s.index})
		}
		s.mu.RUnlock()
	}
	sort.Slice(slots, func(a, b int) bool { return slots[a].idx < slots[b].idx })
	for _, o := range slots {
		o.s.mu.Lock()
		if o.s.index == o.idx {
			c.sealSlotLocked(o.s)
		}
		o.s.mu.Unlock()
	}
}

// Finish seals everything still open, regardless of the seal delay. Call it once, at
// the end of a run, after every runner has stopped.
func (c *Collector) Finish(now time.Duration) {
	_ = now
	for _, s := range c.ring {
		s.mu.Lock()
		if s.index >= 0 {
			c.sealSlotLocked(s)
		}
		s.mu.Unlock()
	}
}

// addPositive files one observation and reports whether it was recorded. A
// logarithmic sketch has no bucket for zero - the library counts it separately, which
// is right for a sub-microsecond operation - and no representation at all for NaN or
// infinity, which can only arrive from a malformed timing.
func addPositive(s *ddsketch.DDSketch, v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	if v < 0 {
		v = 0
	}
	return s.Add(v) == nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func maxOf(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

func firstErr(list ...error) error {
	for _, e := range list {
		if e != nil {
			return e
		}
	}
	return nil
}

func sortedBucketIndexes(m map[int64]*BucketSummary) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
