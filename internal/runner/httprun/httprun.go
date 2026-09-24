// Package httprun drives HTTP requests and records what happened, phase by phase.
//
// This is the application tier: the traffic whose latency users actually feel. Every
// request is timed from the moment the schedule said it should be sent, and httptrace
// breaks the time down into DNS, connect, TLS, waiting for a connection, waiting for
// the first byte and transferring the body. That breakdown is what separates "the
// target is slow" from "our own connection pool is too small".
package httprun

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
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
	"github.com/IshaanNene/Tracepoint/internal/template"
)

// Name is how this runner is registered and how it appears in result.json.
const Name = "http"

// RunIDHeader identifies TracePoint traffic in the target's own logs and APM, so a
// team can filter a load test out of - or into - their dashboards.
const RunIDHeader = "X-Tracepoint-Run-Id"

// PreflightHeader marks the one request per label sent before load starts, so a
// target can leave it out of its own metrics - and so a target that schedules
// anything relative to the run can start its clock at the first real request.
const PreflightHeader = "X-Tracepoint-Preflight"

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
	shared *template.Shared
}

// request is a compiled request: everything resolved once, at load time, so the hot
// path only renders its templates and sends. A part with no template is kept as a
// plain value and costs nothing per request.
type request struct {
	name    string
	method  string
	base    string
	url     string             // resolved, when the url has no template
	urlTpl  *template.Template // set when it does
	host    string             // fixed at load time either way, so policy can judge it
	display string             // the url as written, for messages
	headers map[string]string
	hdrTpls map[string]*template.Template
	body    []byte
	bodyTpl *template.Template
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
	r := &Runner{cfg: cfg, deps: deps, timeout: defaultTimeout, phases: stats, shared: template.NewShared(deps.Clock)}

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
	out := &request{name: src.Name, method: src.Method, base: r.cfg.BaseURL, expect: src.Expect}

	// A template is compiled once, here, so a malformed token or an unknown generator
	// is a configuration error before anything runs, and a token can never be sent
	// literally (§4): a request for /items/{{randInt 1 100}} is a request for a
	// resource that does not exist, and the 404s would look like a target problem.
	urlTpl, err := template.Compile(src.URL)
	if err != nil {
		return nil, withPath(err, "/http/requests/"+src.Name+"/url")
	}
	out.display = src.URL
	if urlTpl.IsStatic() {
		if out.url, err = resolveURL(r.cfg.BaseURL, src.URL); err != nil {
			return nil, err
		}
		if u, perr := url.Parse(out.url); perr == nil {
			out.host = u.Hostname()
		}
	} else {
		// A template may fill in a path or a query, never the scheme or the host: the
		// policy judges every host before anything is contacted, and a host that is
		// only known per request cannot be judged.
		if templatedAuthority(src.URL) {
			return nil, errs.New(errs.CodeConfigInvalidValue,
				"request %q uses a template in its scheme or host", src.Name).
				WithPath("/http/requests/" + src.Name + "/url").
				WithHint("templates may fill a path or query; the host must be fixed so the safety policy can judge it")
		}
		// The static parts must still make a valid url, which a placeholder checks.
		placeholder := strings.ReplaceAll(strings.ReplaceAll(src.URL, "{{", "x"), "}}", "x")
		resolved, rerr := resolveURL(r.cfg.BaseURL, strings.ReplaceAll(placeholder, " ", "x"))
		if rerr != nil {
			return nil, rerr
		}
		if u, perr := url.Parse(resolved); perr == nil {
			out.host = u.Hostname()
		}
		out.urlTpl = urlTpl
	}

	body := []byte(src.Body)
	if src.BodyFile != "" {
		b, err := os.ReadFile(src.BodyFile)
		if err != nil {
			return nil, errs.Wrap(errs.CodeIOReadFailed, err, "reading the body file for request %q", src.Name)
		}
		body = b
	}
	if len(body) > 0 {
		tpl, err := template.Compile(string(body))
		if err != nil {
			return nil, withPath(err, "/http/requests/"+src.Name+"/body")
		}
		if tpl.IsStatic() {
			out.body = body
		} else {
			out.bodyTpl = tpl
		}
	}

	out.headers = make(map[string]string, len(r.cfg.Headers)+len(src.Headers))
	for k, v := range r.cfg.Headers {
		out.headers[k] = v
	}
	for k, v := range src.Headers {
		out.headers[k] = v
	}
	for k, v := range out.headers {
		tpl, err := template.Compile(v)
		if err != nil {
			return nil, withPath(err, "/http/requests/"+src.Name+"/headers/"+k)
		}
		if !tpl.IsStatic() {
			if out.hdrTpls == nil {
				out.hdrTpls = map[string]*template.Template{}
			}
			out.hdrTpls[k] = tpl
			delete(out.headers, k)
		}
	}

	out.timeout = r.timeout
	if src.Timeout != nil {
		out.timeout = src.Timeout.D()
	}
	return out, nil
}

// templatedAuthority reports whether a template token appears before the path of a
// url: in its scheme, credentials, host or port.
func templatedAuthority(raw string) bool {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		if strings.Contains(rest[:i], "{{") {
			return true
		}
		rest = rest[i+3:]
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		return strings.Contains(rest[:end], "{{")
	}
	// A relative url takes its host from base_url; a scheme-relative one does not.
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		return strings.Contains(rest[:end], "{{")
	}
	return false
}

func withPath(err error, path string) error {
	var typed *errs.Error
	if errors.As(err, &typed) && typed.Path == "" {
		return typed.WithPath(path)
	}
	return err
}

// render fills in a request's templates for one iteration. A template that cannot be
// rendered - a variable a journey was meant to extract, say - fails the operation
// rather than sending a literal token.
func (r *Runner) render(req *request, it *runner.Iteration) (target string, body []byte, headers map[string]string, err error) {
	target, body = req.url, req.body
	if req.urlTpl == nil && req.bodyTpl == nil && req.hdrTpls == nil {
		return target, body, nil, nil
	}
	tctx := &template.Context{Shared: r.shared, Rand: it.Rand}
	if req.urlTpl != nil {
		raw, rerr := req.urlTpl.Render(tctx)
		if rerr != nil {
			return "", nil, nil, rerr
		}
		if target, err = resolveURL(req.base, raw); err != nil {
			return "", nil, nil, err
		}
	}
	if req.bodyTpl != nil {
		raw, rerr := req.bodyTpl.Render(tctx)
		if rerr != nil {
			return "", nil, nil, rerr
		}
		body = []byte(raw)
	}
	if req.hdrTpls != nil {
		headers = make(map[string]string, len(req.hdrTpls))
		for k, tpl := range req.hdrTpls {
			v, rerr := tpl.Render(tctx)
			if rerr != nil {
				return "", nil, nil, rerr
			}
			headers[k] = v
		}
	}
	return target, body, headers, nil
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
		if h := req.host; h != "" && !seen[h] {
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

	it := &runner.Iteration{Rand: rand.New(rand.NewPCG(r.deps.Seed, 0))} //nolint:gosec // reproducibility, not secrecy
	target, body, headers, err := r.render(req, it)
	if err != nil {
		return errs.Wrap(errs.CodeConfigTemplateSyntax, err, "rendering request %q for preflight", req.name)
	}
	httpReq, err := r.newRequest(ctx, req, target, body, headers)
	if err != nil {
		return err
	}
	httpReq.Header.Set(PreflightHeader, "1")
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return errs.Wrap(preflightCode(err), err, "preflight request %q to %s failed", req.name, req.display).
			WithHint("check that the target is running and reachable before starting a load test")
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp.Body)

	// A preflight response is reported but not judged: a 404 here is worth knowing
	// about, yet a target legitimately returning 4xx to an unauthenticated probe is
	// not a reason to refuse to run.
	if resp.StatusCode >= 500 {
		return errs.New(errs.CodePreflightHTTP,
			"preflight request %q to %s returned %d", req.name, req.display, resp.StatusCode).
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

	target, body, headers, err := r.render(req, it)
	if err != nil {
		// The iteration stops here: sending the request anyway would mean sending a
		// literal {{token}}, which §4 forbids.
		o.Class = metrics.ClassExtractFailed
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		rec.Record(&o)
		return nil
	}
	httpReq, err := r.newRequest(ctx, req, target, body, headers)
	if err != nil {
		o.Class = metrics.ClassOther
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		rec.Record(&o)
		return nil
	}
	o.BytesOut = int64(len(body))

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

func (r *Runner) newRequest(ctx context.Context, req *request, target string, payload []byte, extra map[string]string) (*http.Request, error) {
	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, body)
	if err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalidValue, err, "building request %q", req.name)
	}
	for k, v := range req.headers {
		httpReq.Header.Set(k, v)
	}
	for k, v := range extra {
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
