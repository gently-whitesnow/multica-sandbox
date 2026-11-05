package opencode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// mcpUpstream records what reached the MCP server and can hold a GET stream open.
type mcpUpstream struct {
	mu      sync.Mutex
	seen    []string
	streams chan struct{}
}

func (u *mcpUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.seen = append(u.seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Mcp-Session-Id"))
	u.mu.Unlock()
	w.Header().Set("Mcp-Session-Id", "mcp-session")
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://idp.example.invalid"`)
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		u.streams <- struct{}{}
		<-r.Context().Done()
		return
	}
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
}

func (u *mcpUpstream) last() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen[len(u.seen)-1]
}

func mcpRelayFixture(t *testing.T) (*MCPRelay, *running, string, *mcpUpstream, *httptest.Server) {
	t.Helper()
	upstream := &mcpUpstream{streams: make(chan struct{}, 1)}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	m := NewMCPRelay()
	proxy, err := relay.New(m, relay.Policy{Allow: MCPRelayPath, Limit: 1 << 20, Reserved: MCPReservedHeaders})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)
	r := &running{task: safeTask(), connections: map[string]Remote{"docs": {Type: "remote", URL: server.URL + "/mcp"}}, tokens: map[string]identity.AccessToken{"docs": identity.NewAccessToken("jwt-one", time.Now().Add(time.Minute))}}
	opaque, err := m.issue(r)
	if err != nil {
		t.Fatal(err)
	}
	return m, r, opaque, upstream, front
}

func mcpRequest(t *testing.T, method, url, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Mcp-Session-Id", "mcp-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMCPRelayForwardsCurrentTokenToExactURL(t *testing.T) {
	_, r, opaque, upstream, front := mcpRelayFixture(t)
	resp := mcpRequest(t, http.MethodPost, front.URL+"/docs", opaque)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || upstream.last() != "POST /mcp Bearer jwt-one mcp-session" || resp.Header.Get("Mcp-Session-Id") != "mcp-session" || resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatalf("forwarded %q status %d header %v", upstream.last(), resp.StatusCode, resp.Header)
	}
	// Renewal replaces the token mid-attempt; the next request carries it.
	r.mu.Lock()
	r.tokens["docs"] = identity.NewAccessToken("jwt-two", time.Now().Add(time.Minute))
	r.mu.Unlock()
	resp = mcpRequest(t, http.MethodPost, front.URL+"/docs", opaque)
	resp.Body.Close()
	if upstream.last() != "POST /mcp Bearer jwt-two mcp-session" {
		t.Fatalf("rotated token not used: %q", upstream.last())
	}
}

func TestMCPRelayAnonymousSendsNoCredential(t *testing.T) {
	_, r, opaque, upstream, front := mcpRelayFixture(t)
	r.anonymous = map[string]bool{"docs": true}
	r.tokens = map[string]identity.AccessToken{}
	resp := mcpRequest(t, http.MethodPost, front.URL+"/docs", opaque)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || upstream.last() != "POST /mcp  mcp-session" {
		t.Fatalf("anonymous forwarded %q status %d", upstream.last(), resp.StatusCode)
	}
	resp = mcpRequest(t, http.MethodPost, front.URL+"/docs", MCPRelayPrefix+strings.Repeat("0", 64))
	resp.Body.Close()
	if resp.StatusCode < 400 || len(upstream.seen) != 1 {
		t.Fatal("anonymous MCP forwarded without the attempt's grant")
	}
}

// All MCP connections share the attempt's grant: one beyond its slots is asked to retry.
func TestMCPRelayFullGrantAsksToRetry(t *testing.T) {
	m, _, opaque, _, front := mcpRelayFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/docs", nil)
	req.Header.Set("Authorization", "Bearer "+opaque)
	for {
		lease, ok := m.Authorize(req)
		if !ok {
			break
		}
		defer lease.Release()
	}
	resp := mcpRequest(t, http.MethodPost, front.URL+"/docs", opaque)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("full MCP grant answered %d", resp.StatusCode)
	}
}

func TestMCPRelayDiscardsCallerQuery(t *testing.T) {
	_, _, opaque, upstream, front := mcpRelayFixture(t)
	for _, query := range []string{"?tenant=other", "?codemode=false", "?"} {
		resp := mcpRequest(t, http.MethodPost, front.URL+"/docs"+query, opaque)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || upstream.last() != "POST /mcp Bearer jwt-one mcp-session" {
			t.Fatalf("query changed the approved URL: %q status %d", upstream.last(), resp.StatusCode)
		}
	}
}

// A live grant's unselected connection is a policy refusal and its lapsed token an
// upstream credential failure; only unknown credentials answer 401.
func TestMCPRelayRefusesUnknownExpiredAndForeign(t *testing.T) {
	_, r, opaque, upstream, front := mcpRelayFixture(t)
	for name, c := range map[string]struct {
		path, bearer string
		status       int
	}{
		"unselected alias": {"/other", opaque, http.StatusForbidden},
		"nested path":      {"/docs/x", opaque, http.StatusForbidden},
		"traversal":        {"/docs/../docs", opaque, http.StatusForbidden},
		"forged":           {"/docs", MCPRelayPrefix + strings.Repeat("0", 64), http.StatusUnauthorized},
		"access token":     {"/docs", "jwt-one", http.StatusUnauthorized},
	} {
		resp := mcpRequest(t, http.MethodPost, front.URL+c.path, c.bearer)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, c.status)
		}
	}
	r.mu.Lock()
	r.tokens["docs"] = identity.NewAccessToken("jwt-old", time.Now())
	r.mu.Unlock()
	resp := mcpRequest(t, http.MethodPost, front.URL+"/docs", opaque)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || len(upstream.seen) != 0 {
		t.Fatalf("expired token: %d, forwarded %v", resp.StatusCode, upstream.seen)
	}
}

func TestMCPRelayRevocationEndsStreams(t *testing.T) {
	m, r, opaque, upstream, front := mcpRelayFixture(t)
	resp := mcpRequest(t, http.MethodGet, front.URL+"/docs", opaque)
	defer resp.Body.Close()
	select {
	case <-upstream.streams:
	case <-time.After(5 * time.Second):
		t.Fatal("GET stream not opened")
	}
	read := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); read <- err }()
	m.revoke(r.task.AttemptKey())
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("revocation left the stream open")
	}
	after := mcpRequest(t, http.MethodPost, front.URL+"/docs", opaque)
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked credential answered %d", after.StatusCode)
	}
}

func TestMCPWithoutRelayIsRejected(t *testing.T) {
	workload := &stubWorkload{}
	a := Adapter{Issuer: &stubIssuer{}, Workloads: workload, Status: stubStatus{}}
	if _, err := a.Start(context.Background(), safeTask()); err == nil || !strings.Contains(err.Error(), "mcp_relay") || workload.writes != 0 {
		t.Fatalf("MCP without relay: %v", err)
	}
}
