// Package guard protects TracePoint's HTTP transports - the REST API and MCP over
// streamable HTTP - the same way (§6.5): a bearer token on every request, a Host
// header that must name the server (DNS-rebinding protection), an Origin header that
// must be absent or explicitly allowed (localhost CSRF protection), and no CORS at all.
//
// A load generator reachable from a web page is a way to make a browser attack
// something, so these checks are not optional and have no switch to turn them off.
package guard

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Options configure the guard.
type Options struct {
	// Token is the bearer token every request must carry.
	Token string
	// Hosts are the Host header values accepted, as host:port. The listen address
	// and its loopback aliases are always accepted.
	Hosts []string
	// Origins are the Origin header values accepted. Empty means a request that
	// carries an Origin at all - which only a browser sends - is refused.
	Origins []string
	// Open lists paths that need no token, such as a health check.
	Open []string
}

// Guard wraps a handler.
type Guard struct {
	token   []byte
	hosts   map[string]bool
	origins map[string]bool
	open    map[string]bool
}

// New builds a guard for a server listening on addr.
func New(addr string, o Options) (*Guard, error) {
	if len(o.Token) < 16 {
		return nil, errs.New(errs.CodeOpsInvalidInput, "the bearer token must be at least 16 characters")
	}
	g := &Guard{token: []byte(o.Token), hosts: map[string]bool{}, origins: map[string]bool{}, open: map[string]bool{}}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errs.Wrap(errs.CodeOpsInvalidInput, err, "%q is not host:port", addr)
	}
	g.hosts[net.JoinHostPort(host, port)] = true
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() || host == "localhost" {
		for _, alias := range []string{"127.0.0.1", "localhost", "::1"} {
			g.hosts[net.JoinHostPort(alias, port)] = true
		}
	}
	for _, h := range o.Hosts {
		g.hosts[strings.ToLower(h)] = true
	}
	for _, origin := range o.Origins {
		g.origins[strings.TrimRight(origin, "/")] = true
	}
	for _, p := range o.Open {
		g.open[p] = true
	}
	return g, nil
}

// Wrap applies every check before next sees the request.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No CORS: a preflight is refused outright, and no response ever carries an
		// Access-Control header.
		if !g.hosts[strings.ToLower(r.Host)] {
			deny(w, http.StatusMisdirectedRequest, errs.New(errs.CodeServerBadHost, "Host %q is not this server", errs.CleanUntrusted(r.Host)).
				WithHint("connect to the address the server listens on; other names are refused against DNS rebinding"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !g.origins[strings.TrimRight(origin, "/")] {
			deny(w, http.StatusForbidden, errs.New(errs.CodeServerBadOrigin, "Origin %q is not allowed", errs.CleanUntrusted(origin)).
				WithHint("browsers may not call this server; a human can allow an origin with --allow-origin"))
			return
		}
		if r.Method == http.MethodOptions {
			deny(w, http.StatusForbidden, errs.New(errs.CodeServerBadOrigin, "cross-origin requests are not supported"))
			return
		}
		if !g.open[r.URL.Path] && !g.authorised(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tracepoint"`)
			deny(w, http.StatusUnauthorized, errs.New(errs.CodeServerUnauthorized, "missing or wrong bearer token").
				WithHint("send Authorization: Bearer <token>; the server printed where its token file is"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Guard) authorised(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), g.token) == 1
}

func deny(w http.ResponseWriter, status int, err error) {
	WriteError(w, status, err)
}

// WriteError writes the error envelope with a status.
func WriteError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	b, merr := json.Marshal(errs.EnvelopeOf(err))
	if merr != nil {
		b = []byte(`{"error":{"code":"INTERNAL","message":"encoding the error"}}`)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return // the client has gone; there is no one left to tell
	}
}

// Token returns the token to use: the one given, or one read from, or generated into,
// a file readable only by its owner.
func Token(given, file string) (token, path string, err error) {
	if given != "" {
		return given, "", nil
	}
	if b, rerr := os.ReadFile(file); rerr == nil { //nolint:gosec // the server's own token file
		if t := strings.TrimSpace(string(b)); len(t) >= 16 {
			return t, file, nil
		}
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return "", "", errs.Wrap(errs.CodeIOReadFailed, rerr, "reading the token file %s", file)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", errs.Wrap(errs.CodeInternal, err, "generating a token")
	}
	token = hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", "", errs.Wrap(errs.CodeIOWriteFailed, err, "creating %s", filepath.Dir(file))
	}
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		return "", "", errs.Wrap(errs.CodeIOWriteFailed, err, "writing the token file %s", file)
	}
	return token, file, nil
}

// CheckBind refuses a non-loopback listen address unless the caller said so
// explicitly: the default is 127.0.0.1, and exposing a load generator is a decision.
func CheckBind(addr string, allowRemote bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errs.Wrap(errs.CodeOpsInvalidInput, err, "%q is not host:port", addr)
	}
	ip := net.ParseIP(host)
	if host == "localhost" || ip != nil && ip.IsLoopback() || allowRemote {
		return nil
	}
	return errs.New(errs.CodePolicyDenied, "refusing to listen on %s, which is not loopback", addr).
		WithHint("listen on 127.0.0.1, or a human can pass --allow-remote to expose it deliberately")
}
