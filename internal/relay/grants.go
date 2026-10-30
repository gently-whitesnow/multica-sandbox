package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gently-whitesnow/multica-sandbox/internal/baseurl"
)

const inflight = 16

// Upstream is the trusted destination of one grant; callers never select any of it.
type Upstream struct {
	Origin     string
	Credential string
	// Header holds trusted attribution set on every forwarded request.
	Header map[string]string
}

// Grants maps opaque per-attempt credentials to upstreams held only in memory.
// A controller restart drops every grant, so recovered attempts fail closed.
type Grants struct {
	prefix    string
	mu        sync.Mutex
	byHash    map[[32]byte]*grant
	byAttempt map[string][32]byte
}

type grant struct {
	attempt    string
	origin     *url.URL
	credential string
	header     http.Header
	ctx        context.Context
	cancel     context.CancelFunc
	slots      chan struct{}
	// status is the attempt's last upstream response status.
	status atomic.Int32
}

// NewGrants issues credentials of the form prefix + 64 hex characters.
func NewGrants(prefix string) *Grants {
	return &Grants{prefix: prefix, byHash: map[[32]byte]*grant{}, byAttempt: map[string][32]byte{}}
}

// Issue binds a new opaque credential to an active attempt; one grant per attempt.
func (g *Grants) Issue(attempt string, u Upstream) (string, error) {
	origin, err := parseOrigin(u.Origin)
	if attempt == "" || err != nil || u.Credential == "" || strings.ContainsAny(u.Credential, "\r\n") {
		return "", errors.New("relay grant requires an attempt, origin and credential")
	}
	return g.issue(attempt, origin, u)
}

// Bind issues an opaque credential without an upstream: its leases carry no origin, so
// the relay refuses them unless an authorizer resolves the upstream per request.
func (g *Grants) Bind(attempt string) (string, error) {
	if attempt == "" {
		return "", errors.New("relay grant requires an attempt")
	}
	return g.issue(attempt, nil, Upstream{})
}

func (g *Grants) issue(attempt string, origin *url.URL, u Upstream) (string, error) {
	header := http.Header{}
	for name, value := range u.Header {
		if name == "" || strings.ContainsAny(name+value, "\r\n\x00") {
			return "", errors.New("invalid relay attribution header")
		}
		header.Set(name, value)
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("relay credential generation failed")
	}
	opaque := g.prefix + hex.EncodeToString(random)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.byAttempt[attempt]; ok {
		return "", errors.New("attempt already has a relay grant")
	}
	ctx, cancel := context.WithCancel(context.Background())
	hash := sha256.Sum256([]byte(opaque))
	g.byHash[hash] = &grant{attempt: attempt, origin: origin, credential: u.Credential, header: header, ctx: ctx, cancel: cancel, slots: make(chan struct{}, inflight)}
	g.byAttempt[attempt] = hash
	return opaque, nil
}

// Revoke denies new requests and cancels in-flight ones; it is idempotent.
func (g *Grants) Revoke(attempt string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	hash, ok := g.byAttempt[attempt]
	if !ok {
		return
	}
	g.byHash[hash].cancel()
	delete(g.byHash, hash)
	delete(g.byAttempt, attempt)
}

// Responded records an upstream response status for the attempt.
func (g *Grants) Responded(attempt string, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if hash, ok := g.byAttempt[attempt]; ok {
		g.byHash[hash].status.Store(int32(status))
	}
}

// Refusal reports the attempt's last upstream status when it was a refusal, else 0: an
// agent that reports a provider error without its status is classified by it.
func (g *Grants) Refusal(attempt string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	hash, ok := g.byAttempt[attempt]
	if !ok {
		return 0
	}
	if status := int(g.byHash[hash].status.Load()); status >= 400 {
		return status
	}
	return 0
}

// Authorize accepts the opaque credential as a bearer token or, as Anthropic clients
// send it, in X-Api-Key; a request carrying both must repeat the same credential.
func (g *Grants) Authorize(r *http.Request) (Lease, bool) {
	opaque, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	keys := r.Header.Values("X-Api-Key")
	switch {
	case len(keys) > 1 || (bearer && len(keys) == 1 && keys[0] != opaque) || (!bearer && r.Header.Get("Authorization") != ""):
		return Lease{}, false
	case !bearer && len(keys) == 1:
		opaque = keys[0]
	}
	if !strings.HasPrefix(opaque, g.prefix) || len(opaque) != len(g.prefix)+64 {
		return Lease{}, false
	}
	g.mu.Lock()
	entry := g.byHash[sha256.Sum256([]byte(opaque))]
	g.mu.Unlock()
	if entry == nil {
		return Lease{}, false
	}
	select {
	case entry.slots <- struct{}{}:
	default:
		return Lease{}, false
	}
	return Lease{Attempt: entry.attempt, Origin: entry.origin, Credential: entry.credential, Header: entry.header, Context: entry.ctx, Release: func() { <-entry.slots }}, true
}

// parseOrigin admits an HTTP(S) origin with an optional base path, such as a
// service published under a shared ingress prefix; requests are joined below it.
func parseOrigin(origin string) (*url.URL, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !baseurl.Path(u) {
		return nil, errors.New("relay origin must be an HTTP(S) origin")
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimSuffix(u.Path, "/")}, nil
}

// reserved reports whether a header name starts with one of the lower-case prefixes.
func reserved(name string, prefixes []string) bool {
	name = strings.ToLower(name)
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
