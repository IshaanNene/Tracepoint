package httprun

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
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
