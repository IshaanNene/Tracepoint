// Package metrics records outcomes on the hot path and summarises them.
//
// Per (label, open bucket) it keeps relative-error sketches and per-class counters,
// seals buckets into fixed-size summaries once nothing in flight can still land in
// them, and merges the sketches into whole-run ones. Memory is therefore independent
// of run length. See docs/adr/002-sketches-and-memory.md.
//
// Three latency views are recorded for every operation, and keeping them apart is
// what lets the analysis distinguish a slow target from a slow generator:
//
//	response_time = end - intended        what a user feels; SLOs are judged on it
//	service_time  = end - connection      what the target did; attribution uses it
//	client_wait   = connection - intended our own dispatch, queue and pool delay
package metrics

import "time"

// Class is how an operation ended. The set is fixed and its String values are the
// keys used in result.json, so adding one is an additive schema change and readers
// are told to tolerate unknown keys.
type Class uint8

// Outcome classes.
const (
	ClassOK            Class = iota // succeeded and met every expectation
	ClassTimeout                    // exceeded its deadline
	ClassConnection                 // refused, reset or closed before a response
	ClassDNS                        // name resolution failed
	ClassTLS                        // handshake or certificate failure
	ClassPoolTimeout                // no connection came free in time: a generator-side limit
	ClassHTTP4xx                    // a 4xx outside the expected set
	ClassHTTP5xx                    // a 5xx outside the expected set
	ClassExpectFailed               // arrived, but failed a status or JSON expectation
	ClassExtractFailed              // a value a later step needed could not be extracted
	ClassQueryError                 // the database or Redis server returned an error
	ClassCanceled                   // cut short by shutdown, not by the target
	ClassOther                      // unclassified

	numClasses = int(ClassOther) + 1
)

//nolint:gochecknoglobals // A fixed lookup table, never mutated after init.
var classNames = [numClasses]string{
	ClassOK:            "ok",
	ClassTimeout:       "timeout",
	ClassConnection:    "connection",
	ClassDNS:           "dns",
	ClassTLS:           "tls",
	ClassPoolTimeout:   "pool_timeout",
	ClassHTTP4xx:       "http_4xx",
	ClassHTTP5xx:       "http_5xx",
	ClassExpectFailed:  "expect_failed",
	ClassExtractFailed: "extract_failed",
	ClassQueryError:    "query_error",
	ClassCanceled:      "canceled",
	ClassOther:         "other",
}

// String returns the snake_case name used in result.json.
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "other"
}

// IsError reports whether the operation failed.
func (c Class) IsError() bool { return c != ClassOK }

// CountsTowardAbortGuard reports whether this class counts towards the abort guard.
//
// Connection errors, timeouts and 5xx do: they mean the target is failing. 4xx and
// 429 do not, because they are the target working as designed - aborting a run
// because a service correctly rejected unauthorised requests would be wrong (spec §8).
// Generator-side classes are excluded too: the guard protects the target, and our own
// pool exhaustion says nothing about its health.
func (c Class) CountsTowardAbortGuard() bool {
	switch c {
	case ClassTimeout, ClassConnection, ClassDNS, ClassTLS, ClassHTTP5xx:
		return true
	case ClassOK, ClassPoolTimeout, ClassHTTP4xx, ClassExpectFailed,
		ClassExtractFailed, ClassQueryError, ClassCanceled, ClassOther:
		return false
	default:
		return false
	}
}

// Classes returns every class in order, for building complete counter tables.
func Classes() []Class {
	out := make([]Class, numClasses)
	for i := range out {
		out[i] = Class(i)
	}
	return out
}

// LabelID identifies a label within one runner. Labels are resolved to integers once,
// when the runner prepares, so the record path never hashes a string. It is an int
// rather than a narrower type because it is only ever a slice index, and narrowing
// would buy nothing while adding conversions that have to be justified.
type LabelID int

// Outcome is one completed operation. Every time is an offset from the single
// monotonic run start shared by every runner and sampler.
//
// Outcome is passed by pointer to Record and is not retained: a caller may reuse the
// same struct for every operation, which is what keeps the hot path allocation-free.
type Outcome struct {
	Label  LabelID
	Class  Class
	Status int32 // HTTP status; 0 for non-HTTP runners

	// Intended is when the scheduler said this operation should be sent. Measuring
	// from here rather than from Dispatched is what makes latency
	// coordinated-omission correct: a target that stalls cannot hide by throttling us.
	Intended time.Duration
	// Dispatched is when a worker was handed the arrival.
	Dispatched time.Duration
	// WorkerStart is when the worker began the operation.
	WorkerStart time.Duration
	// ConnAcquired is when a connection was in hand and the target could be asked.
	ConnAcquired time.Duration
	// FirstByte is when the first response byte arrived. HTTP only; zero otherwise.
	FirstByte time.Duration
	// End is when the operation finished, successfully or not.
	End time.Duration

	BytesIn  int64
	BytesOut int64
}

// ResponseTime is what a user would feel: the wait from when the request was due to
// be sent until it finished. This is what SLOs are judged on.
func (o *Outcome) ResponseTime() time.Duration { return o.End - o.Intended }

// ServiceTime is what the target actually did, with our own queueing removed. This is
// what tier attribution uses, because charging a database for time spent waiting
// inside our process would be wrong.
func (o *Outcome) ServiceTime() time.Duration { return o.End - o.ConnAcquired }

// ClientWait is the generator's own delay: dispatch lag plus queue wait plus pool
// wait. When this dominates a hot bucket the finding is about us, not the target.
func (o *Outcome) ClientWait() time.Duration { return o.ConnAcquired - o.Intended }

// DispatchLag is how late the arrival was handed out relative to its scheduled time.
// A high p99 here means the generator missed its own schedule and the run is invalid.
func (o *Outcome) DispatchLag() time.Duration { return o.Dispatched - o.Intended }

// Recorder is the write side of the collector, handed to runners so they can record
// an outcome without reaching the rest of the collector's surface (spec §3.2).
type Recorder interface {
	Record(o *Outcome)
}
