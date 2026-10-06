// Package relay is the controller's narrow credential-translation boundary
// (ADR 0012 and 0014): one trusted origin per relay, caller authorization is
// replaced, redirects are refused and upstream details never reach the caller.
package relay

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"
)

// Lease is an authorized forwarding grant for one request.
type Lease struct {
	Credential string
	// Context ends when the grant is revoked, cancelling in-flight requests.
	Context context.Context
	Release func()
}

// Authorizer validates the caller and resolves the upstream credential from trusted state.
type Authorizer interface {
	Authorize(*http.Request) (Lease, bool)
}

type Relay struct {
	auth  Authorizer
	allow func(string) bool
	limit int64
	proxy *httputil.ReverseProxy
}

type credentialKey struct{}

var errRedirect = errors.New("upstream redirect refused")

// dropped never leave the relay: caller credentials, sessions and forwarding hints.
var dropped = []string{"Authorization", "Cookie", "Proxy-Authorization", "Forwarded", "X-Real-Ip", "X-Actor-Source", "X-User-Id", "X-User-Email"}

// New forwards to origin only; allow admits cleaned request paths.
func New(origin string, auth Authorizer, allow func(string) bool, limit int64) (*Relay, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || auth == nil || allow == nil || limit <= 0 {
		return nil, errors.New("relay requires an HTTP(S) origin, authorizer, path policy and body limit")
	}
	u.Path = ""
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
	r := &Relay{auth: auth, allow: allow, limit: limit}
	r.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			for _, name := range dropped {
				pr.Out.Header.Del(name)
			}
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
					pr.Out.Header.Del(name)
				}
			}
			pr.Out.Header.Set("Authorization", "Bearer "+pr.In.Context().Value(credentialKey{}).(string))
		},
		ModifyResponse: func(resp *http.Response) error {
			// The transport never follows redirects; refusing them also withholds upstream locations.
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return errRedirect
			}
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			deny(w, http.StatusBadGateway, "upstream unavailable")
		},
	}
	return r, nil
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Scheme != "" || req.URL.Host != "" || req.Method == http.MethodConnect || req.Header.Get("Upgrade") != "" || !cleanPath(req.URL) || !r.allow(req.URL.Path) {
		deny(w, http.StatusForbidden, "request not permitted")
		return
	}
	lease, ok := r.auth.Authorize(req)
	if !ok {
		deny(w, http.StatusUnauthorized, "invalid relay credential")
		return
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(context.WithValue(req.Context(), credentialKey{}, lease.Credential))
	defer cancel()
	defer context.AfterFunc(lease.Context, cancel)()
	req.Body = http.MaxBytesReader(w, req.Body, r.limit)
	r.proxy.ServeHTTP(w, req.WithContext(ctx))
}

// cleanPath rejects traversal, empty segments and encoded separators the upstream router could decode.
func cleanPath(u *url.URL) bool {
	p := u.Path
	escaped := strings.ToLower(u.EscapedPath())
	if !strings.HasPrefix(p, "/") || strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%2e") || strings.Contains(escaped, "%5c") || strings.Contains(p, "\\") {
		return false
	}
	trimmed := strings.TrimSuffix(p, "/")
	return trimmed == "" || path.Clean(trimmed) == trimmed
}

func deny(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+message+`"}`)
}
