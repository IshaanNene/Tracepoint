package httprun

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/DataDog/sketches-go/ddsketch/mapping"
	"github.com/DataDog/sketches-go/ddsketch/store"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
)

// phaseTrace records where the time inside one request went.
//
// The breakdown is what separates a slow service from a slow client. Time spent
// waiting for a connection from our own pool, or resolving a name, is ours; time
// between sending the request and the first byte coming back is theirs. Charging the
// first to the target would make an undersized pool look like a database problem.
type phaseTrace struct {
	clock clock.Clock
	start time.Time

	mu           sync.Mutex
	getConn      time.Duration
	gotConn      time.Duration
	dnsStart     time.Duration
	dnsDone      time.Duration
	connectStart time.Duration
	connectDone  time.Duration
	tlsStart     time.Duration
	tlsDone      time.Duration
	firstByte    time.Duration
	reused       bool
	haveConn     bool
}

func (p *phaseTrace) now() time.Duration { return p.clock.Now().Sub(p.start) }

// clientTrace builds the hooks. The callbacks run on whichever goroutine the transport
// happens to be using, so the fields are guarded.
func (p *phaseTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:              func(string) { p.set(&p.getConn) },
		DNSStart:             func(httptrace.DNSStartInfo) { p.set(&p.dnsStart) },
		DNSDone:              func(httptrace.DNSDoneInfo) { p.set(&p.dnsDone) },
		ConnectStart:         func(string, string) { p.set(&p.connectStart) },
		ConnectDone:          func(string, string, error) { p.set(&p.connectDone) },
		TLSHandshakeStart:    func() { p.set(&p.tlsStart) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { p.set(&p.tlsDone) },
		GotFirstResponseByte: func() { p.set(&p.firstByte) },
		GotConn: func(info httptrace.GotConnInfo) {
			t := p.now()
			p.mu.Lock()
			p.gotConn = t
			p.reused = info.Reused
			p.haveConn = true
			p.mu.Unlock()
		},
	}
}

func (p *phaseTrace) set(field *time.Duration) {
	t := p.now()
	p.mu.Lock()
	*field = t
	p.mu.Unlock()
}

// connAcquiredOr reports when a connection was in hand, falling back to the supplied
// value when the request failed before one ever was.
func (p *phaseTrace) connAcquiredOr(fallback time.Duration) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.haveConn {
		return p.gotConn
	}
	return fallback
}

// Phases is the per-request breakdown, all offsets from the run start.
type Phases struct {
	DNS       time.Duration
	Connect   time.Duration
	TLS       time.Duration
	ConnWait  time.Duration
	TTFB      time.Duration
	Transfer  time.Duration
	Reused    bool
	Connected bool
}

// phases reduces the stamps to durations. end is when the whole operation finished.
func (p *phaseTrace) phases(end time.Duration) Phases {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := Phases{Reused: p.reused, Connected: p.haveConn}
	if p.dnsDone > p.dnsStart {
		out.DNS = p.dnsDone - p.dnsStart
	}
	if p.connectDone > p.connectStart {
		out.Connect = p.connectDone - p.connectStart
	}
	if p.tlsDone > p.tlsStart {
		out.TLS = p.tlsDone - p.tlsStart
	}
	if p.gotConn > p.getConn {
		out.ConnWait = p.gotConn - p.getConn
	}
	if p.firstByte > p.gotConn && p.haveConn {
		out.TTFB = p.firstByte - p.gotConn
	}
	if end > p.firstByte && p.firstByte > 0 {
		out.Transfer = end - p.firstByte
	}
	return out
}

// phaseStats aggregates the per-request breakdown over a whole run.
//
// Whole-run rather than per-bucket, deliberately: six more sketches for every (label,
// bucket) pair would dominate the memory budget in ADR-002, and the breakdown is read
// as a single summary in the report. The per-request stamps that feed it are still
// recorded on every outcome.
type phaseStats struct {
	mu       sync.Mutex
	dns      *phaseSeries
	connect  *phaseSeries
	tlsPhase *phaseSeries
	connWait *phaseSeries
	ttfb     *phaseSeries
	transfer *phaseSeries

	requests int64
	reused   int64
}

func newPhaseStats() (*phaseStats, error) {
	s := &phaseStats{}
	for _, target := range []**phaseSeries{&s.dns, &s.connect, &s.tlsPhase, &s.connWait, &s.ttfb, &s.transfer} {
		series, err := newPhaseSeries()
		if err != nil {
			return nil, err
		}
		*target = series
	}
	return s, nil
}

func (s *phaseStats) observe(p Phases) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if p.Reused {
		s.reused++
	}
	// A phase that did not happen - no DNS lookup on a reused connection - is not
	// recorded as a zero, which would drag every percentile towards nothing and hide
	// how expensive a cold connection actually is.
	if p.DNS > 0 {
		s.dns.add(p.DNS)
	}
	if p.Connect > 0 {
		s.connect.add(p.Connect)
	}
	if p.TLS > 0 {
		s.tlsPhase.add(p.TLS)
	}
	if p.ConnWait > 0 {
		s.connWait.add(p.ConnWait)
	}
	if p.TTFB > 0 {
		s.ttfb.add(p.TTFB)
	}
	if p.Transfer > 0 {
		s.transfer.add(p.Transfer)
	}
}

// PhaseQuantiles is the request breakdown in milliseconds.
type PhaseQuantiles struct {
	DNS      metrics.Quantiles `json:"dns"`
	Connect  metrics.Quantiles `json:"connect"`
	TLS      metrics.Quantiles `json:"tls"`
	ConnWait metrics.Quantiles `json:"conn_wait"`
	TTFB     metrics.Quantiles `json:"ttfb"`
	Transfer metrics.Quantiles `json:"transfer"`
}

func (s *phaseStats) quantiles() PhaseQuantiles {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PhaseQuantiles{
		DNS: s.dns.quantiles(), Connect: s.connect.quantiles(), TLS: s.tlsPhase.quantiles(),
		ConnWait: s.connWait.quantiles(), TTFB: s.ttfb.quantiles(), Transfer: s.transfer.quantiles(),
	}
}

func (s *phaseStats) reuseRatio() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requests == 0 {
		return 0
	}
	return float64(s.reused) / float64(s.requests)
}

// phaseSeries is one phase's distribution.
type phaseSeries struct {
	sketch *ddsketch.DDSketch
	n      int64
	sum    float64
	max    float64
}

func newPhaseSeries() (*phaseSeries, error) {
	m, err := mapping.NewLogarithmicMapping(metrics.DefaultRelativeAccuracy)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "building a phase sketch")
	}
	return &phaseSeries{sketch: ddsketch.NewDDSketch(m, store.NewDenseStore(), store.NewDenseStore())}, nil
}

func (p *phaseSeries) add(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 0 || p.sketch.Add(ms) != nil {
		return
	}
	p.n++
	p.sum += ms
	if ms > p.max {
		p.max = ms
	}
}

func (p *phaseSeries) quantiles() metrics.Quantiles {
	q := metrics.Quantiles{Max: p.max}
	if p.n > 0 {
		q.Mean = p.sum / float64(p.n)
	}
	if p.sketch.GetCount() == 0 {
		return q
	}
	read := func(x float64) float64 {
		v, err := p.sketch.GetValueAtQuantile(x)
		if err != nil || v < 0 {
			return 0
		}
		return v
	}
	q.P50, q.P90, q.P95, q.P99, q.P999 = read(0.5), read(0.9), read(0.95), read(0.99), read(0.999)
	return q
}
