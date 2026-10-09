// Package relay is the controller's narrow credential-translation boundary
// (ADR 0012 and 0014): each grant fixes one trusted origin, caller authorization is
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
	"slices"
	"strconv"
	"strings"
	"time"
)

// Lease is an authorized forwarding grant for one request.
type Lease struct {
	// Attempt names the grant's attempt for authorizers that add per-request policy.
	Attempt    string
	Origin     *url.URL
	Credential string
	// Authorization, when set, replaces the bearer built from Credential; Path replaces the request path.
	Authorization, Path string
	Header              http.Header
	// Context ends when the grant is revoked, cancelling in-flight requests.
	Context context.Context
	Release func()
}

// Authorizer validates the caller and resolves the upstream from trusted state.
type Authorizer interface {
	Authorize(*http.Request) (Lease, bool)
}

// Policy bounds what a relay forwards.
type Policy struct {
	// Allow admits cleaned request paths.
	Allow func(string) bool
	Limit int64
	// Reserved lists lower-case header prefixes owned by the relay: caller values
	// are removed before forwarding and upstream values before responding.
	Reserved []string
	// Withhold replaces upstream error bodies with a fixed one; the status is kept.
	Withhold bool
	// HeaderTimeout bounds the wait for upstream response headers (default 60s).
	HeaderTimeout time.Duration
	// Proxy admits only absolute-form http targets, as an HTTP proxy receives them;
	// the authorizer resolves the upstream from the target host.
	Proxy bool
}

type Relay struct {
	auth   Authorizer
	policy Policy
	proxy  *httputil.ReverseProxy
}

type leaseKey struct{}

var errRedirect = errors.New("upstream redirect refused")

// dropped never leave the relay: caller credentials, sessions and forwarding hints.
var dropped = []string{"Authorization", "Cookie", "Proxy-Authorization", "Forwarded", "X-Real-Ip", "X-Actor-Source", "X-User-Id", "X-User-Email"}

func New(auth Authorizer, p Policy) (*Relay, error) {
	if auth == nil || p.Allow == nil || p.Limit <= 0 || p.HeaderTimeout < 0 {
		return nil, errors.New("relay requires an authorizer, path policy and body limit")
	}
	if p.HeaderTimeout == 0 {
		p.HeaderTimeout = 60 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: p.HeaderTimeout,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
	r := &Relay{auth: auth, policy: p}
	r.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			lease := pr.In.Context().Value(leaseKey{}).(Lease)
			pr.SetURL(lease.Origin)
			if lease.Path != "" {
				pr.Out.URL.Path, pr.Out.URL.RawPath = lease.Path, ""
			}
			for _, name := range dropped {
				pr.Out.Header.Del(name)
			}
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") || reserved(name, p.Reserved) {
					pr.Out.Header.Del(name)
				}
			}
			for name, values := range lease.Header {
				pr.Out.Header[name] = slices.Clone(values)
			}
			switch {
			case lease.Authorization != "":
				pr.Out.Header.Set("Authorization", lease.Authorization)
			case lease.Credential != "":
				pr.Out.Header.Set("Authorization", "Bearer "+lease.Credential)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// The transport never follows redirects; refusing them also withholds upstream locations.
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return errRedirect
			}
			resp.Header.Del("Set-Cookie")
			for name := range resp.Header {
				if reserved(name, p.Reserved) {
					resp.Header.Del(name)
				}
			}
			if p.Withhold && resp.StatusCode >= 400 {
				_ = resp.Body.Close()
				body := `{"error":{"message":"upstream refused the request","type":"upstream_error","code":"` + strconv.Itoa(resp.StatusCode) + `"}}`
				resp.Body = io.NopCloser(strings.NewReader(body))
				resp.ContentLength = int64(len(body))
				resp.Header = http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Content-Length": {strconv.Itoa(len(body))}}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			deny(w, http.StatusBadGateway, "upstream unavailable")
		},
	}
	return r, nil
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	absolute := req.URL.Scheme != "" || req.URL.Host != ""
	if absolute != r.policy.Proxy || (absolute && (req.URL.Scheme != "http" || req.URL.User != nil)) || req.Method == http.MethodConnect || req.Header.Get("Upgrade") != "" || !cleanPath(req.URL) || !r.policy.Allow(req.URL.Path) {
		deny(w, http.StatusForbidden, "request not permitted")
		return
	}
	lease, ok := r.auth.Authorize(req)
	if !ok {
		deny(w, http.StatusUnauthorized, "invalid relay credential")
		return
	}
	defer lease.Release()
	if lease.Origin == nil || (lease.Path != "" && !strings.HasPrefix(lease.Path, "/")) {
		deny(w, http.StatusForbidden, "request not permitted")
		return
	}
	ctx, cancel := context.WithCancel(context.WithValue(req.Context(), leaseKey{}, lease))
	defer cancel()
	defer context.AfterFunc(lease.Context, cancel)()
	req.Body = http.MaxBytesReader(w, req.Body, r.policy.Limit)
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
