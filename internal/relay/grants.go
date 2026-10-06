package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// Prefix keeps the upstream CLI's task-token check; the suffix is longer than a real mat_ token.
const Prefix = "mat_relay_"

const inflight = 16

// Grants maps opaque per-attempt credentials to upstream credentials held only in memory.
// A controller restart drops every grant, so recovered attempts fail closed.
type Grants struct {
	mu        sync.Mutex
	byHash    map[[32]byte]*grant
	byAttempt map[string][32]byte
}

type grant struct {
	credential string
	ctx        context.Context
	cancel     context.CancelFunc
	slots      chan struct{}
}

func NewGrants() *Grants {
	return &Grants{byHash: map[[32]byte]*grant{}, byAttempt: map[string][32]byte{}}
}

// Issue binds a new opaque credential to an active attempt; one grant per attempt.
func (g *Grants) Issue(attempt, credential string) (string, error) {
	if attempt == "" || credential == "" || strings.ContainsAny(credential, "\r\n") {
		return "", errors.New("relay grant requires an attempt and credential")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("relay credential generation failed")
	}
	opaque := Prefix + hex.EncodeToString(random)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.byAttempt[attempt]; ok {
		return "", errors.New("attempt already has a relay grant")
	}
	ctx, cancel := context.WithCancel(context.Background())
	hash := sha256.Sum256([]byte(opaque))
	g.byHash[hash] = &grant{credential: credential, ctx: ctx, cancel: cancel, slots: make(chan struct{}, inflight)}
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

func (g *Grants) Authorize(r *http.Request) (Lease, bool) {
	opaque, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(opaque, Prefix) || len(opaque) != len(Prefix)+64 {
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
	return Lease{Credential: entry.credential, Context: entry.ctx, Release: func() { <-entry.slots }}, true
}
