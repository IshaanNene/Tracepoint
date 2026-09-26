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
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
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
//
// Every iteration runs one journey. A plain `requests:` list is compiled into
// single-step journeys (ADR-003), so extraction, expectations, cookies, feeders and
// think time have exactly one implementation, and a user who wrote `requests` never
// sees the word journey: a one-step journey's label is the request's name.
type Runner struct {
	cfg    *config.HTTP
	deps   runner.Deps
	client *http.Client

	journeys []*journey
	picker   *runner.Weighted
	feeders  map[string]*feeder
	timeout  time.Duration
	insecure bool
	cookies  bool

	phases        *phaseStats
	shared        *template.Shared
	exhaustedOnce sync.Once
}

// journey is a compiled journey: its steps in order, and what it needs per iteration.
type journey struct {
	name    string
	steps   []*request
	feeders []string // the feeders its templates read
	extract bool     // whether any step extracts, so the iteration needs a variable map
}

// request is a compiled step: everything resolved once, at load time, so the hot
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
	extract []config.Extract
	think   *config.ThinkTime
	// needBody is set when an expectation or an extraction reads the body, which is
	// then kept (up to a bound) rather than only drained.
	needBody bool
	label    metrics.LabelID
	labelStr string
}

// New builds an HTTP runner from an already-validated configuration.
func New(cfg *config.HTTP, runDuration, defaultTimeout time.Duration, deps runner.Deps) (*Runner, error) {
	if cfg == nil {
		return nil, errs.New(errs.CodeInternal, "the http runner needs a configuration")
	}
	stats, err := newPhaseStats()
	if err != nil {
		return nil, err
	}
	r := &Runner{
		cfg: cfg, deps: deps, timeout: defaultTimeout, phases: stats,
		shared: template.NewShared(deps.Clock), feeders: map[string]*feeder{},
		cookies: cfg.Cookies == nil || *cfg.Cookies,
	}
	for i, f := range cfg.Feeders {
		fd, ferr := loadFeeder(f, fmt.Sprintf("/http/feeders/%d", i))
		if ferr != nil {
			return nil, ferr
		}
		r.feeders[f.Name] = fd
	}

	// requests desugar to one-step journeys named after the request.
	type source struct {
		name   string
		weight float64
		path   string
		steps  []config.HTTPStep
		single bool
	}
	var sources []source
	for i := range cfg.Requests {
		q := cfg.Requests[i]
		sources = append(sources, source{name: q.Name, weight: q.WeightOr(), path: fmt.Sprintf("/http/requests/%d", i), steps: []config.HTTPStep{q}, single: true})
	}
	for i := range cfg.Journeys {
		j := &cfg.Journeys[i]
		sources = append(sources, source{name: j.Name, weight: j.WeightOr(), path: fmt.Sprintf("/http/journeys/%d", i), steps: j.Steps})
	}

	weights := make([]float64, 0, len(sources))
	for _, src := range sources {
		j := &journey{name: src.name}
		used := map[string]bool{}
		vars := map[string]bool{}
		for k := range src.steps {
			step := &src.steps[k]
			path := src.path
			if !src.single {
				path = fmt.Sprintf("%s/steps/%d", src.path, k)
			}
			compiled, cerr := r.compile(step, path, vars, used)
			if cerr != nil {
				return nil, cerr
			}
			compiled.labelStr = step.Name
			if !src.single {
				compiled.labelStr = src.name + "/" + step.Name
			}
			compiled.label = deps.Collector.LabelID(compiled.labelStr)
			j.steps = append(j.steps, compiled)
			for _, e := range step.Extract {
				vars[e.Name] = true
				j.extract = true
			}
		}
		for name := range used {
			j.feeders = append(j.feeders, name)
		}
		sort.Strings(j.feeders)
		r.journeys = append(r.journeys, j)
		weights = append(weights, src.weight)
	}
	if len(r.journeys) == 0 {
		return nil, errs.New(errs.CodeConfigMissingField, "the http section has no requests and no journeys").WithPath("/http")
	}
	r.picker = runner.NewWeighted(weights)

	client, insecure, err := buildClient(cfg, runDuration)
	if err != nil {
		return nil, err
	}
	r.client, r.insecure = client, insecure
	return r, nil
}

// compile builds one step. vars are the variables earlier steps of its journey
// extract, and used collects the feeders it reads.
func (r *Runner) compile(src *config.HTTPStep, path string, vars, used map[string]bool) (*request, error) {
	out := &request{
		name: src.Name, method: src.Method, base: r.cfg.BaseURL, expect: src.Expect,
		extract: src.Extract, think: src.Think,
	}
	if out.method == "" {
		out.method = http.MethodGet
	}
	for _, e := range src.Extract {
		if e.From == "" || e.From == "body" {
			out.needBody = true
		}
	}
	if src.Expect != nil && len(src.Expect.JSON) > 0 {
		out.needBody = true
	}
	var templates []*template.Template
	note := func(t *template.Template) { templates = append(templates, t) }

	// A template is compiled once, here, so a malformed token or an unknown generator
	// is a configuration error before anything runs, and a token can never be sent
	// literally (§4): a request for /items/{{randInt 1 100}} is a request for a
	// resource that does not exist, and the 404s would look like a target problem.
	urlTpl, err := template.Compile(src.URL)
	if err != nil {
		return nil, withPath(err, path+"/url")
	}
	note(urlTpl)
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
				WithPath(path + "/url").
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
		// The file is only read here, so the dataflow check the loader applies to an
		// inline body is applied to it now.
		feeders := map[string]bool{}
		for name := range r.feeders {
			feeders[name] = true
		}
		if cerr := config.CheckTemplate(string(b), path+"/body_file",
			config.Scope{Owner: fmt.Sprintf("the body file of %q", src.Name), Vars: vars, Feeders: feeders, Journey: true}); cerr != nil {
			return nil, cerr
		}
		body = b
	}
	if len(body) > 0 {
		tpl, err := template.Compile(string(body))
		if err != nil {
			return nil, withPath(err, path+"/body")
		}
		note(tpl)
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
			return nil, withPath(err, path+"/headers/"+k)
		}
		note(tpl)
		if !tpl.IsStatic() {
			if out.hdrTpls == nil {
				out.hdrTpls = map[string]*template.Template{}
			}
			out.hdrTpls[k] = tpl
			delete(out.headers, k)
		}
	}

	// Every feeder column a template reads must exist in the file, which is only
	// known now that the file has been read.
	for _, t := range templates {
		for _, fc := range t.FeederColumns() {
			name, col, _ := strings.Cut(fc, ".")
			fd, ok := r.feeders[name]
			if !ok {
				// The loader's dataflow check already refused this configuration;
				// reaching here means it was built by hand, so fail the same way.
				return nil, errs.New(errs.CodeConfigFeederNotFound, "request %q reads from feeder %q, which is not declared", src.Name, name).WithPath(path)
			}
			if _, ok := fd.columns[col]; !ok {
				return nil, errs.New(errs.CodeConfigFeederNotFound, "request %q reads column %q of feeder %q, which has no such column", src.Name, col, name).
					WithPath(path).WithHint("the feeder's columns are %s", strings.Join(sortedColumns(fd), ", "))
			}
			used[name] = true
		}
	}

	out.timeout = r.timeout
	if src.Timeout != nil {
		out.timeout = src.Timeout.D()
	}
	return out, nil
}

func sortedColumns(fd *feeder) []string {
	out := make([]string, 0, len(fd.columns))
	for c := range fd.columns {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
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
func (r *Runner) render(req *request, rng *rand.Rand, vars map[string]string, rows map[string]map[string]string) (target string, body []byte, headers map[string]string, err error) {
	target, body = req.url, req.body
	if req.urlTpl == nil && req.bodyTpl == nil && req.hdrTpls == nil {
		return target, body, nil, nil
	}
	tctx := &template.Context{Shared: r.shared, Rand: rng, Vars: vars, Rows: rows}
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

// Labels are the step names - a request's own name, or journey/step - known before
// the run starts.
func (r *Runner) Labels() []string {
	var out []string
	for _, j := range r.journeys {
		for _, st := range j.steps {
			out = append(out, st.labelStr)
		}
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
	for _, j := range r.journeys {
		for _, st := range j.steps {
			if h := st.host; h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// Prepare runs every journey once, in order, before any load starts.
//
// Preflight costs one round trip per step and catches the things that would otherwise
// waste a whole run: a wrong port, an unresolvable host, a path that 404s because of a
// typo - and, for a journey, an extraction path that does not match what the target
// returns, which would otherwise fail every iteration at its second step.
func (r *Runner) Prepare(ctx context.Context) error {
	rng := rand.New(rand.NewPCG(r.deps.Seed, 0)) //nolint:gosec // reproducibility, not secrecy
	for _, j := range r.journeys {
		if err := r.preflight(ctx, j, rng); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) preflight(ctx context.Context, j *journey, rng *rand.Rand) error {
	rows, ok := r.rowsFor(j, rng)
	if !ok {
		return errs.New(errs.CodeConfigInvalidValue, "a unique feeder of journey %q has no rows left before the run started", j.name)
	}
	vars := map[string]string{}
	var jar http.CookieJar
	if r.cookies {
		jar = newJar()
	}
	for _, st := range j.steps {
		var o metrics.Outcome
		res := r.exchange(ctx, st, rng, vars, rows, jar, "", true, &o)
		switch {
		case res.renderErr != nil:
			return errs.Wrap(errs.CodeConfigTemplateSyntax, res.renderErr, "rendering %q for preflight", st.labelStr)
		case res.transportErr != nil:
			return errs.Wrap(preflightCode(res.transportErr), res.transportErr, "preflight request %q to %s failed", st.labelStr, st.display).
				WithHint("check that the target is running and reachable before starting a load test")
		case res.status >= 500:
			// A preflight response is reported but not judged below 500: a 404 here is
			// worth knowing about, yet a target legitimately returning 4xx to an
			// unauthenticated probe is not a reason to refuse to run.
			return errs.New(errs.CodePreflightHTTP,
				"preflight request %q to %s returned %d", st.labelStr, st.display, res.status).
				WithHint("the target is failing before the test has started")
		case res.missing != "":
			return errs.New(errs.CodePreflightHTTP,
				"preflight: step %q got %d, but its extraction %q found nothing", st.labelStr, res.status, res.missing).
				WithHint("check the extraction's path against what the target actually returns; every iteration would fail at the next step")
		}
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

// Do runs one journey and records an outcome for each step it reaches.
//
// The first step is timed from the iteration's intended send time, so the open model
// stays coordinated-omission correct. Every later step is timed from when it actually
// started: think time between steps is the simulated user's, not the target's, and is
// excluded from every latency. A step that fails ends the iteration; the steps after
// it are not sent, because they would depend on what it did not return.
func (r *Runner) Do(ctx context.Context, it *runner.Iteration, rec metrics.Recorder) error {
	j := r.journeys[it.Session.Choose(func() int { return r.picker.Pick(it.Rand) })]

	var vars map[string]string
	if j.extract {
		vars = make(map[string]string, 4)
	}
	jar := r.jarFor(it.Session, len(j.steps) > 1)
	trace := ""
	if r.cfg.Traceparent {
		trace = traceID(it.Rand)
	}

	rows, ok := r.rowsFor(j, it.Rand)
	if !ok {
		// A unique feeder has run out. The first step cannot be given the value it
		// needs, which is what extract_failed means; nothing is sent.
		r.exhaustedOnce.Do(func() {
			if r.deps.Logger != nil {
				r.deps.Logger.Warn("a unique feeder ran out of rows; further iterations of the journey fail as extract_failed", "journey", j.name)
			}
		})
		var o metrics.Outcome
		it.StampOutcome(&o)
		o.Label = j.steps[0].label
		o.Class = metrics.ClassExtractFailed
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		rec.Record(&o)
		return nil
	}

	last := len(j.steps) - 1
	for k, st := range j.steps {
		var o metrics.Outcome
		if k == 0 {
			it.StampOutcome(&o)
		} else {
			now := r.deps.Elapsed()
			o.Intended, o.Dispatched, o.WorkerStart = now, now, now
		}
		r.exchange(ctx, st, it.Rand, vars, rows, jar, trace, false, &o)
		rec.Record(&o)
		if o.Class != metrics.ClassOK || ctx.Err() != nil {
			return nil
		}
		if k < last && st.think != nil {
			if err := r.pause(ctx, st.think, it.Rand); err != nil {
				return nil
			}
		}
	}
	return nil
}

// result is what one exchange reports besides the outcome, for preflight's messages.
type result struct {
	status       int
	renderErr    error
	transportErr error
	missing      string // the extraction that found nothing
}

// exchange sends one step and fills in the outcome: rendering against the journey's
// variables and feeder rows, cookies, the request, expectations and extraction.
func (r *Runner) exchange(ctx context.Context, st *request, rng *rand.Rand, vars map[string]string,
	rows map[string]map[string]string, jar http.CookieJar, trace string, preflight bool, o *metrics.Outcome) result {
	o.Label = st.label

	tr := &phaseTrace{start: r.deps.Start, clock: r.deps.Clock}
	ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, tr.clientTrace()), st.timeout)
	defer cancel()

	target, body, headers, err := r.render(st, rng, vars, rows)
	if err != nil {
		// The iteration stops here: sending the request anyway would mean sending a
		// literal {{token}}, which §4 forbids.
		o.Class = metrics.ClassExtractFailed
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		return result{renderErr: err}
	}
	httpReq, err := r.newRequest(ctx, st, target, body, headers)
	if err != nil {
		o.Class = metrics.ClassOther
		o.ConnAcquired = r.deps.Elapsed()
		o.End = o.ConnAcquired
		return result{transportErr: err}
	}
	if jar != nil {
		for _, c := range jar.Cookies(httpReq.URL) {
			httpReq.AddCookie(c)
		}
	}
	if trace != "" {
		httpReq.Header.Set("traceparent", traceparent(trace, rng))
	}
	if preflight {
		httpReq.Header.Set(PreflightHeader, "1")
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
		return result{transportErr: err}
	}

	o.Status = int32(resp.StatusCode) //nolint:gosec // an HTTP status fits in an int32
	o.ConnAcquired = tr.connAcquiredOr(o.WorkerStart)
	o.FirstByte = tr.firstByte

	var kept []byte
	var n int64
	var readErr error
	if st.needBody {
		kept, n, readErr = capture(resp.Body, st.expect)
	} else {
		n, readErr = drain(resp.Body, st.expect)
	}
	_ = resp.Body.Close()
	if jar != nil {
		if cs := resp.Cookies(); len(cs) > 0 {
			jar.SetCookies(resp.Request.URL, cs)
		}
	}
	o.BytesIn = n
	o.End = r.deps.Elapsed()
	if !preflight {
		// Where the time went inside the request. Aggregated for the whole run rather
		// than per bucket: six more sketches per (label, bucket) would cost far more
		// memory than the breakdown is worth, and it is read as a whole-run figure.
		r.phases.observe(tr.phases(o.End))
	}

	res := result{status: resp.StatusCode}
	switch {
	case readErr != nil:
		o.Class = classifyTransportError(ctx, readErr)
		res.transportErr = readErr
		return res
	default:
		o.Class = classifyResponse(resp.StatusCode, st.expect, n)
	}
	if o.Class == metrics.ClassOK && st.expect != nil && len(st.expect.JSON) > 0 && !jsonHolds(kept, st.expect.JSON) {
		o.Class = metrics.ClassExpectFailed
	}
	// Extraction happens only from a response that counted as a success: a value
	// pulled out of an error page would send the next step somewhere meaningless.
	if o.Class == metrics.ClassOK {
		for _, e := range st.extract {
			v, found := extractValue(e, kept, resp)
			if !found {
				o.Class = metrics.ClassExtractFailed
				res.missing = e.Name
				break
			}
			if vars != nil {
				vars[e.Name] = v
			}
		}
	}
	return res
}

// pause sleeps for a step's think time: constant, uniform between min and max, or
// exponential around a mean (capped at ten means, so one unlucky draw cannot stall a
// user for the rest of the run).
func (r *Runner) pause(ctx context.Context, t *config.ThinkTime, rng *rand.Rand) error {
	var d time.Duration
	switch t.Type {
	case "constant":
		if t.Duration != nil {
			d = t.Duration.D()
		}
	case "uniform":
		if t.Min != nil && t.Max != nil {
			lo, hi := t.Min.D(), t.Max.D()
			d = lo + time.Duration(rng.Float64()*float64(hi-lo))
		}
	case "exponential":
		if t.Mean != nil {
			mean := float64(t.Mean.D())
			d = time.Duration(min(rng.ExpFloat64()*mean, 10*mean))
		}
	}
	if d <= 0 {
		return nil
	}
	return r.deps.Clock.SleepUntil(ctx, r.deps.Clock.Now().Add(d))
}

// rowsFor picks this iteration's row from every feeder the journey reads. A journey
// sees one row per feeder for all of its steps, so a user logs in and checks out as
// the same person.
func (r *Runner) rowsFor(j *journey, rng *rand.Rand) (map[string]map[string]string, bool) {
	if len(j.feeders) == 0 {
		return nil, true
	}
	rows := make(map[string]map[string]string, len(j.feeders))
	for _, name := range j.feeders {
		row, ok := r.feeders[name].pick(rng)
		if !ok {
			return nil, false
		}
		rows[name] = row
	}
	return rows, true
}

// jarFor returns the cookie jar for a user: the session's own under the closed
// model, which lasts the run; a fresh one for this iteration under the open model,
// and only when there is a later step for a cookie to reach.
func (r *Runner) jarFor(s *runner.Session, multiStep bool) http.CookieJar {
	if !r.cookies {
		return nil
	}
	if s == nil {
		if !multiStep {
			return nil
		}
		return newJar()
	}
	if jar, ok := s.Data.(http.CookieJar); ok {
		return jar
	}
	jar := newJar()
	s.Data = jar
	return jar
}

func newJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil // cookiejar.New fails only for a bad public suffix list, and there is none
	}
	return jar
}

// traceID draws a W3C trace id for one iteration: every step of a journey shares it,
// so an APM shows the journey as one trace.
func traceID(rng *rand.Rand) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], rng.Uint64())
	binary.BigEndian.PutUint64(b[8:], rng.Uint64()|1) // never all zeros, which W3C forbids
	return hex.EncodeToString(b[:])
}

// traceparent is the header for one step: the iteration's trace, a new span, sampled.
func traceparent(trace string, rng *rand.Rand) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], rng.Uint64()|1)
	return "00-" + trace + "-" + hex.EncodeToString(b[:]) + "-01"
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
