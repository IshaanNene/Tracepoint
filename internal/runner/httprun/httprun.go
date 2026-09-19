// Package httprun drives HTTP requests and records what happened, phase by phase.
//
// This is the application tier: the traffic whose latency users actually feel. Every
// request is timed from the moment the schedule said it should be sent, and httptrace
// breaks the time down into DNS, connect, TLS, waiting for a connection, waiting for
// the first byte and transferring the body. That breakdown is what separates "the
// target is slow" from "our own connection pool is too small".
package httprun

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/buildinfo"
	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
	"github.com/IshaanNene/Tracepoint/internal/metrics"
	"github.com/IshaanNene/Tracepoint/internal/runner"
)

// Name is how this runner is registered and how it appears in result.json.
const Name = "http"

// RunIDHeader identifies TracePoint traffic in the target's own logs and APM, so a
// team can filter a load test out of - or into - their dashboards.
const RunIDHeader = "X-Tracepoint-Run-Id"

// discardBufferSize bounds how much is read at a time while draining a body. Bodies
// are always read to completion and closed, both so the connection can be reused and
// so transfer time is measured rather than skipped.
const discardBufferSize = 32 * 1024

// Runner drives HTTP load.
type Runner struct {
	cfg    *config.HTTP
	deps   runner.Deps
	client *http.Client

	requests []*request
	picker   *runner.Weighted
	labels   []metrics.LabelID
	timeout  time.Duration
	insecure bool

	phases *phaseStats
}

// request is a compiled request: everything resolved once, at load time, so the hot
// path only fills in a body and sends.
type request struct {
	name    string
	method  string
	url     string
	headers map[string]string
	body    []byte
	timeout time.Duration
	expect  *config.Expect
	label   metrics.LabelID
}

// New builds an HTTP runner from an already-validated configuration.
func New(cfg *config.HTTP, runDuration, defaultTimeout time.Duration, deps runner.Deps) (*Runner, error) {
	if cfg == nil {
		return nil, errs.New(errs.CodeInternal, "the http runner needs a configuration")
	}
	if len(cfg.Journeys) > 0 {
		return nil, errs.New(errs.CodeConfigInvalidValue,
			"multi-step journeys are not in this build yet").
			WithPath("/http/journeys").
			WithHint("use `requests:` for now; journeys land in phase 6")
	}

	stats, err := newPhaseStats()
	if err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg, deps: deps, timeout: defaultTimeout, phases: stats}

	weights := make([]float64, 0, len(cfg.Requests))
	for i := range cfg.Requests {
		src := &cfg.Requests[i]
		compiled, compileErr := r.compile(src)
		if compileErr != nil {
			return nil, compileErr
		}
		r.requests = append(r.requests, compiled)
		r.labels = append(r.labels, deps.Collector.LabelID(src.Name))
		weights = append(weights, src.WeightOr())
	}
	for i, c := range r.requests {
		c.label = r.labels[i]
	}
	r.picker = runner.NewWeighted(weights)

	client, insecure, err := buildClient(cfg, runDuration)
	if err != nil {
		return nil, err
	}
	r.client, r.insecure = client, insecure
	return r, nil
}

func (r *Runner) compile(src *config.HTTPStep) (*request, error) {
	full, err := resolveURL(r.cfg.BaseURL, src.URL)
	if err != nil {
		return nil, err
	}
	// Templates are compiled at load time in phase 2. Until then a token would be sent
	// literally, which §4 forbids outright - a request for /items/{{randInt 1 100}} is
	// a request for a resource that does not exist, and the resulting 404s would look
	// like a target problem.
	for what, value := range map[string]string{"url": src.URL, "body": src.Body} {
		if strings.Contains(value, "{{") {
			return nil, errs.New(errs.CodeConfigTemplateSyntax,
				"request %q uses a template in its %s, which this build cannot expand yet", src.Name, what).
				WithPath("/http/requests").
				WithHint("templates and generators land in phase 2; use a literal value for now")
		}
	}

	body := []byte(src.Body)
	if src.BodyFile != "" {
		b, err := os.ReadFile(src.BodyFile)
		if err != nil {
			return nil, errs.Wrap(errs.CodeIOReadFailed, err, "reading the body file for request %q", src.Name)
		}
		body = b
	}

	headers := make(map[string]string, len(r.cfg.Headers)+len(src.Headers))
	for k, v := range r.cfg.Headers {
		headers[k] = v
	}
	for k, v := range src.Headers {
		headers[k] = v
	}

	timeout := r.timeout
	if src.Timeout != nil {
		timeout = src.Timeout.D()
	}
	return &request{
		name: src.Name, method: src.Method, url: full,
		headers: headers, body: body, timeout: timeout, expect: src.Expect,
	}, nil
}

func resolveURL(base, ref string) (string, error) {
	if base == "" {
		u, err := url.Parse(ref)
		if err != nil {
			return "", errs.Wrap(errs.CodeConfigInvalidValue, err, "%q is not a valid url", ref)
		}
		if !u.IsAbs() {
			return "", errs.New(errs.CodeConfigInvalidValue, "%q is not an absolute url", ref).
				WithHint("set http.base_url, or write the full url including the scheme")
		}
		return u.String(), nil
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", errs.Wrap(errs.CodeConfigInvalidValue, err, "%q is not a valid base_url", base)
	}
	u, err := b.Parse(ref)
	if err != nil {
		return "", errs.Wrap(errs.CodeConfigInvalidValue, err, "%q is not a valid url under base %q", ref, base)
	}
	return u.String(), nil
}

// buildClient tunes the transport so the client is not itself the bottleneck. An idle
// pool smaller than the concurrency would silently turn every request into a fresh
// connection, and the run would measure connection setup rather than the service.
func buildClient(cfg *config.HTTP, _ time.Duration) (*http.Client, bool, error) {
	inFlight := cfg.Executor.MaxInFlight
	if inFlight <= 0 {
		inFlight = 256
	}
	idlePerHost := inFlight
	http2, keepAlive := true, true
	var tlsCfg *tls.Config
	insecure := false

	if t := cfg.Transport; t != nil {
		if t.MaxIdleConnsPerHost > 0 {
			idlePerHost = t.MaxIdleConnsPerHost
		}
		if t.HTTP2 != nil {
			http2 = *t.HTTP2
		}
		if t.KeepAlive != nil {
			keepAlive = *t.KeepAlive
		}
		if t.TLS != nil {
			c, err := buildTLS(t.TLS)
			if err != nil {
				return nil, false, err
			}
			tlsCfg, insecure = c, t.TLS.InsecureSkipVerify
		}
	}

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          idlePerHost * 2,
		MaxIdleConnsPerHost:   idlePerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     http2,
		DisableKeepAlives:     !keepAlive,
		TLSClientConfig:       tlsCfg,
	}
	// No Client.Timeout: the deadline is set per request on the context, so a
	// per-request override is possible and a timeout is distinguishable from a
	// cancelled run.
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}, insecure, nil
}

func buildTLS(c *config.TLS) (*tls.Config, error) {
	//nolint:gosec // InsecureSkipVerify is opt-in, gated by policy, and printed in every report.
	out := &tls.Config{
		InsecureSkipVerify: c.InsecureSkipVerify,
		ServerName:         c.ServerName,
		MinVersion:         tls.VersionTLS12,
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, errs.Wrap(errs.CodeIOReadFailed, err, "reading the CA bundle %s", c.CAFile)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errs.New(errs.CodeConfigInvalidValue, "%s contains no usable certificates", c.CAFile)
		}
		out.RootCAs = pool
	}
	if c.CertFile != "" || c.KeyFile != "" {
		pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, errs.Wrap(errs.CodeConfigInvalidValue, err, "loading the client certificate")
		}
		out.Certificates = []tls.Certificate{pair}
	}
	return out, nil
}

// SetStart fixes the run's monotonic origin. The engine calls it once, after preflight
// and before any load, because every offset a runner records is measured from it.
func (r *Runner) SetStart(t time.Time) { r.deps.Start = t }

// Name identifies the runner.
func (r *Runner) Name() string { return Name }

// Kind reports that this is the tier users talk to.
func (r *Runner) Kind() metrics.Kind { return metrics.KindApp }

// Labels are the request names, known before the run starts.
func (r *Runner) Labels() []string {
	out := make([]string, 0, len(r.requests))
	for _, req := range r.requests {
		out = append(out, req.name)
	}
	return out
}

// InsecureTLS reports whether certificate verification was disabled, so the result can
// say so.
func (r *Runner) InsecureTLS() bool { return r.insecure }

// Targets lists the distinct hosts this runner will contact.
func (r *Runner) Targets() []string {
	seen := map[string]bool{}
	var out []string
	for _, req := range r.requests {
		u, err := url.Parse(req.url)
		if err != nil {
			continue
		}
		if h := u.Hostname(); h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// Prepare sends one request per label before any load starts.
//
// Preflight costs one round trip per request and catches the things that would
// otherwise waste a whole run: a wrong port, an unresolvable host, a path that 404s
// because of a typo. A failure here is reported before a single measurement is taken.
func (r *Runner) Prepare(ctx context.Context) error {
	for _, req := range r.requests {
		if err := r.preflight(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) preflight(ctx context.Context, req *request) error {
	ctx, cancel := context.WithTimeout(ctx, req.timeout)
	defer cancel()

	httpReq, err := r.build(ctx, req)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return errs.Wrap(preflightCode(err), err, "preflight request %q to %s failed", req.name, req.url).
			WithHint("check that the target is running and reachable before starting a load test")
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp.Body)

	// A preflight response is reported but not judged: a 404 here is worth knowing
	// about, yet a target legitimately returning 4xx to an unauthenticated probe is
	// not a reason to refuse to run.
	if resp.StatusCode >= 500 {
		return errs.New(errs.CodePreflightHTTP,
			"preflight request %q to %s returned %d", req.name, req.url, resp.StatusCode).
			WithHint("the target is failing before the test has started")
	}
	return nil
}

func preflightCode(err error) errs.Code {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errs.CodePreflightDNS
	}
	return errs.CodePreflightConnect
}

// Do performs one request and records its outcome.
func (r *Runner) Do(ctx context.Context, it *runner.Iteration, rec metrics.Recorder) error {
	req := r.requests[r.picker.Pick(it.Rand)]

	var o metrics.Outcome
	it.StampOutcome(&o)
	o.Label = req.label

	tr := &phaseTrace{start: r.deps.Start, clock: r.deps.Clock}
	ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, tr.clientTrace()), req.timeout)
	defer cancel()

	httpReq, err := r.build(ctx, req)
	if err != nil {
		o.Class = metrics.ClassOther
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		rec.Record(&o)
		return nil
	}
	o.BytesOut = int64(len(req.body))

	resp, err := r.client.Do(httpReq)
	if err != nil {
		// The connection was never established, or the exchange failed. Attribute the
		// time to the point a connection was in hand if we got that far, so a
		// connection failure is not charged to the target as service time.
		o.ConnAcquired = tr.connAcquiredOr(r.deps.Elapsed())
		o.End = r.deps.Elapsed()
		o.Class = classifyTransportError(ctx, err)
		rec.Record(&o)
		return nil
	}

	o.Status = int32(resp.StatusCode) //nolint:gosec // an HTTP status fits in an int32
	o.ConnAcquired = tr.connAcquiredOr(o.WorkerStart)
	o.FirstByte = tr.firstByte

	n, readErr := drain(resp.Body, req.expect)
	_ = resp.Body.Close()
	o.BytesIn = n
	o.End = r.deps.Elapsed()
	// Where the time went inside the request. Aggregated for the whole run rather than
	// per bucket: six more sketches per (label, bucket) would cost far more memory than
	// the breakdown is worth, and it is read as a whole-run figure anyway.
	r.phases.observe(tr.phases(o.End))

	switch {
	case readErr != nil:
		o.Class = classifyTransportError(ctx, readErr)
	default:
		o.Class = classifyResponse(resp.StatusCode, req.expect, n)
	}
	rec.Record(&o)
	return nil
}

func (r *Runner) build(ctx context.Context, req *request) (*http.Request, error) {
	var body io.Reader
	if len(req.body) > 0 {
		body = strings.NewReader(string(req.body))
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, req.url, body)
	if err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalidValue, err, "building request %q", req.name)
	}
	for k, v := range req.headers {
		httpReq.Header.Set(k, v)
	}
	// Identifiable traffic: a team looking at their own logs should be able to tell a
	// load test from real users, and filter it either way.
	httpReq.Header.Set("User-Agent", buildinfo.UserAgent(r.deps.RunID))
	httpReq.Header.Set(RunIDHeader, r.deps.RunID)
	return httpReq, nil
}

// drain reads the body to completion and reports its size. Always: an undrained body
// cannot be reused by the connection pool, and skipping the read would leave transfer
// time out of the measurement.
func drain(body io.Reader, expect *config.Expect) (int64, error) {
	limit := int64(-1)
	if expect != nil && expect.MaxBodyBytes > 0 {
		limit = expect.MaxBodyBytes
	}
	buf := make([]byte, discardBufferSize)
	var total int64
	for {
		n, err := body.Read(buf)
		total += int64(n)
		if limit > 0 && total > limit {
			// Keep draining so the connection stays reusable, but remember the breach.
			discard(body)
			return total, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// discard reads the rest of a body and throws it away.
//
// The error is deliberately not returned. The only reason to keep reading at this
// point is so the connection can go back to the pool; if the read fails the
// connection is discarded instead, which is the same outcome by another route, and the
// operation's own classification has already been decided.
func discard(r io.Reader) {
	_, _ = io.Copy(io.Discard, r) //nolint:errcheck // see above: nothing to do with it
}

// classifyResponse decides whether a response counts as a success.
//
// Without an explicit expectation, 2xx and 3xx pass. With one, anything outside the
// listed statuses fails - and it is classified by what the status actually was, so
// that the abort guard can count 5xx while ignoring 4xx, and so that a run measuring a
// rate limiter shows up as 429s rather than as generic failures.
func classifyResponse(status int, expect *config.Expect, bodyBytes int64) metrics.Class {
	if expect != nil && expect.MaxBodyBytes > 0 && bodyBytes > expect.MaxBodyBytes {
		return metrics.ClassExpectFailed
	}
	if expect != nil && len(expect.Status) > 0 {
		for _, want := range expect.Status {
			if status == want {
				return metrics.ClassOK
			}
		}
		return statusClass(status)
	}
	if status >= 200 && status < 400 {
		return metrics.ClassOK
	}
	return statusClass(status)
}

func statusClass(status int) metrics.Class {
	switch {
	case status >= 500:
		return metrics.ClassHTTP5xx
	case status >= 400:
		return metrics.ClassHTTP4xx
	default:
		return metrics.ClassExpectFailed
	}
}

// classifyTransportError sorts a failure into the taxonomy the abort guard and the
// validity rules read. The distinction that matters most is between a deadline the
// target blew and a cancellation we caused: only the first says anything about them.
func classifyTransportError(ctx context.Context, err error) metrics.Class {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return metrics.ClassTimeout
	case errors.Is(err, context.Canceled):
		// A cancelled parent context is our shutdown, not their failure.
		if ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
			return metrics.ClassCanceled
		}
		return metrics.ClassCanceled
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return metrics.ClassDNS
	}
	var certErr *x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var recErr tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &recErr) {
		return metrics.ClassTLS
	}
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &certInvalid) {
		return metrics.ClassTLS
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return metrics.ClassTimeout
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "no such host"),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF):
		return metrics.ClassConnection
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return metrics.ClassTLS
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return metrics.ClassConnection
	}
	return metrics.ClassOther
}

// PhaseBreakdown reports where request time went, aggregated over the run. It is what
// distinguishes a slow service from an undersized connection pool.
func (r *Runner) PhaseBreakdown() PhaseQuantiles { return r.phases.quantiles() }

// ConnectionReuse is the share of requests served on a reused connection. A low value
// with keep-alive on usually means the idle pool is smaller than the concurrency, and
// the run is measuring connection setup.
func (r *Runner) ConnectionReuse() float64 { return r.phases.reuseRatio() }

// Close releases idle connections.
func (r *Runner) Close() error {
	if tr, ok := r.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

var _ runner.Runner = (*Runner)(nil)

func init() {
	runner.Register(Name, func(cfg any, deps runner.Deps) (runner.Runner, error) {
		c, ok := cfg.(*runnerConfig)
		if !ok {
			return nil, errs.New(errs.CodeInternal, "the http runner was given %T", cfg)
		}
		return New(c.HTTP, c.RunDuration, c.Timeout, deps)
	})
}

// runnerConfig is what the engine passes through the registry.
type runnerConfig struct {
	HTTP        *config.HTTP
	RunDuration time.Duration
	Timeout     time.Duration
}

// Config packages what the registry needs to build this runner.
func Config(h *config.HTTP, runDuration, timeout time.Duration) any {
	return &runnerConfig{HTTP: h, RunDuration: runDuration, Timeout: timeout}
}
