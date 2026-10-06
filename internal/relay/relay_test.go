package relay

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const upstreamA = "mat_0000000000000000000000000000000000000a"
const upstreamB = "mat_0000000000000000000000000000000000000b"
const prefix = multica.RelayPrefix

type fixture struct {
	grants   *Grants
	relay    *httptest.Server
	origin   string
	attacker atomic.Int32
	target   string
	seen     chan *http.Request
}

func newFixture(t *testing.T, upstream http.HandlerFunc, p Policy) *fixture {
	t.Helper()
	f := &fixture{grants: NewGrants(prefix), seen: make(chan *http.Request, 16)}
	attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { f.attacker.Add(1) }))
	t.Cleanup(attacker.Close)
	f.target = attacker.URL
	if upstream == nil {
		upstream = func(w http.ResponseWriter, r *http.Request) {
			f.seen <- r.Clone(r.Context())
			http.SetCookie(w, &http.Cookie{Name: "multica_auth", Value: "session"})
			w.Header().Set("X-Litellm-Model-Api-Base", "http://provider.internal")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}
	origin := httptest.NewServer(upstream)
	t.Cleanup(origin.Close)
	f.origin = origin.URL
	if p.Allow == nil {
		p = Policy{Allow: multica.RelayPath, Limit: 1024}
	}
	handler, err := New(f.grants, p)
	if err != nil {
		t.Fatal(err)
	}
	f.relay = httptest.NewServer(handler)
	t.Cleanup(f.relay.Close)
	return f
}

func (f *fixture) issue(t *testing.T, attempt, credential string) string {
	t.Helper()
	opaque, err := f.grants.Issue(attempt, Upstream{Origin: f.origin, Credential: credential})
	if err != nil {
		t.Fatal(err)
	}
	return opaque
}

func (f *fixture) do(t *testing.T, method, path, bearer string, body io.Reader, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, f.relay.URL+path, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestTranslatesCredentialToGrantOrigin(t *testing.T) {
	f := newFixture(t, nil, Policy{})
	a := f.issue(t, "attempt-a", upstreamA)
	if !strings.HasPrefix(a, prefix) || strings.Contains(a, upstreamA) {
		t.Fatal("unsafe opaque credential")
	}
	b := f.issue(t, "attempt-b", upstreamB)
	resp, body := f.do(t, http.MethodGet, "/api/issues/x?output=json", a, nil, map[string]string{"Cookie": "multica_auth=stolen", "X-Forwarded-Host": "attacker", "X-User-Email": "forged@example.invalid", "Host": strings.TrimPrefix(f.target, "http://")})
	if resp.StatusCode != 200 || body != `{"ok":true}` || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("forward failed or leaked session: %d %s", resp.StatusCode, resp.Header)
	}
	got := <-f.seen
	if got.Header.Get("Authorization") != "Bearer "+upstreamA || got.Header.Get("Cookie") != "" || got.Header.Get("X-Forwarded-Host") != "" || got.Header.Get("X-User-Email") != "" || got.URL.RawQuery != "output=json" {
		t.Fatalf("upstream request not translated: %v", got.Header)
	}
	f.do(t, http.MethodGet, "/api/issues/y", b, nil, nil)
	if got := <-f.seen; got.Header.Get("Authorization") != "Bearer "+upstreamB {
		t.Fatal("attempt credential crossed to another attempt")
	}
	if f.attacker.Load() != 0 {
		t.Fatal("caller-selected host contacted")
	}
}

func TestGrantSelectsOriginAndAttribution(t *testing.T) {
	other := make(chan *http.Request, 4)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other <- r.Clone(r.Context()) }))
	t.Cleanup(second.Close)
	f := newFixture(t, nil, Policy{Allow: func(p string) bool { return p == "/v1/chat/completions" }, Limit: 1024, Reserved: []string{"x-litellm-"}})
	a, err := f.grants.Issue("attempt-a", Upstream{Origin: f.origin, Credential: "sk-workspace-a", Header: map[string]string{"X-Litellm-End-User-Id": "ws-a/agent/task"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.grants.Issue("attempt-b", Upstream{Origin: second.URL + "/", Credential: "sk-workspace-b"})
	resp, _ := f.do(t, http.MethodPost, "/v1/chat/completions", a, strings.NewReader(`{"user":"spoofed"}`), map[string]string{"X-Litellm-Customer-Id": "spoofed", "x-litellm-end-user-id": "spoofed", "X-LiteLLM-Tags": "spoofed"})
	got := <-f.seen
	if got.Header.Get("Authorization") != "Bearer sk-workspace-a" || got.Header.Get("X-Litellm-End-User-Id") != "ws-a/agent/task" || len(got.Header.Values("X-Litellm-End-User-Id")) != 1 || got.Header.Get("X-Litellm-Customer-Id") != "" || got.Header.Get("X-Litellm-Tags") != "" {
		t.Fatalf("caller attribution not replaced: %v", got.Header)
	}
	if resp.Header.Get("X-Litellm-Model-Api-Base") != "" {
		t.Fatal("reserved upstream response header leaked")
	}
	f.do(t, http.MethodPost, "/v1/chat/completions", b, strings.NewReader(`{}`), nil)
	if got := <-other; got.Header.Get("Authorization") != "Bearer sk-workspace-b" || got.Header.Get("X-Litellm-End-User-Id") != "" {
		t.Fatal("grant did not select its own origin and credential")
	}
	if len(f.seen) != 0 {
		t.Fatal("workspace credential reached another workspace's gateway")
	}
	for _, bad := range []Upstream{{Origin: "http://gateway.invalid/v1", Credential: "k"}, {Origin: "ftp://gateway.invalid", Credential: "k"}, {Origin: "http://u:p@gateway.invalid", Credential: "k"}, {Origin: f.origin, Credential: "k\r\nX: y"}, {Origin: f.origin, Credential: "k", Header: map[string]string{"X-A": "b\nc"}}} {
		if _, err := f.grants.Issue("attempt-bad", bad); err == nil {
			t.Fatalf("unsafe upstream accepted: %+v", bad.Origin)
		}
	}
}

func TestEndedWrongAndUpstreamCredentialsDenied(t *testing.T) {
	f := newFixture(t, nil, Policy{})
	a := f.issue(t, "attempt-a", upstreamA)
	b := f.issue(t, "attempt-b", upstreamB)
	if _, err := f.grants.Issue("attempt-b", Upstream{Origin: f.origin, Credential: upstreamB}); err == nil {
		t.Fatal("second grant for one attempt accepted")
	}
	f.grants.Revoke("attempt-a")
	f.grants.Revoke("attempt-a")
	for _, bearer := range []string{a, upstreamA, prefix + strings.Repeat("0", 64), "", b + "x", strings.ToUpper(b)} {
		if resp, _ := f.do(t, http.MethodGet, "/api/issues/x", bearer, nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("credential %q accepted: %d", bearer, resp.StatusCode)
		}
	}
	if resp, _ := f.do(t, http.MethodGet, "/api/issues/x", b, nil, nil); resp.StatusCode != 200 {
		t.Fatal("revoking one attempt affected another")
	}
	<-f.seen
	// A restarted controller has new, empty grants: earlier credentials fail closed.
	restarted, _ := New(NewGrants(prefix), Policy{Allow: multica.RelayPath, Limit: 1024})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/issues/x", nil)
	req.Header.Set("Authorization", "Bearer "+b)
	restarted.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || len(f.seen) != 0 {
		t.Fatal("credential survived controller restart")
	}
}

func TestRefusesRedirectsAndWithholdsErrors(t *testing.T) {
	var target atomic.Value
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			w.Header().Set("X-Litellm-Key-Spend", "1")
			w.WriteHeader(422)
			_, _ = io.WriteString(w, "Budget exceeded Key=sk-...K7QQ")
			return
		}
		http.Redirect(w, r, target.Load().(string)+"/collect", http.StatusTemporaryRedirect)
	}
	f := newFixture(t, upstream, Policy{})
	target.Store(f.target)
	a := f.issue(t, "attempt-a", upstreamA)
	resp, body := f.do(t, http.MethodPost, "/api/issues/x", a, strings.NewReader("{}"), nil)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" || strings.Contains(body, f.target) || strings.Contains(body, upstreamA) {
		t.Fatalf("redirect not refused: %d %s %s", resp.StatusCode, resp.Header, body)
	}
	if f.attacker.Load() != 0 {
		t.Fatal("redirect followed")
	}
	// Multica API errors pass through unchanged; transport errors are generic.
	if resp, body := f.do(t, http.MethodGet, "/api/fail", a, nil, nil); resp.StatusCode != 422 || !strings.Contains(body, "Budget exceeded") {
		t.Fatal("upstream status not preserved")
	}
	withheld := newFixture(t, upstream, Policy{Allow: func(string) bool { return true }, Limit: 1024, Reserved: []string{"x-litellm-"}, Withhold: true})
	w := withheld.issue(t, "attempt-w", "sk-workspace")
	resp, body = withheld.do(t, http.MethodPost, "/v1/fail", w, strings.NewReader("{}"), nil)
	if resp.StatusCode != 422 || strings.Contains(body, "K7QQ") || !strings.Contains(body, `"code":"422"`) || resp.Header.Get("X-Litellm-Key-Spend") != "" {
		t.Fatalf("gateway error not withheld: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	f.grants.Revoke("attempt-a")
	broken, _ := f.grants.Issue("attempt-a", Upstream{Origin: "http://127.0.0.1:1", Credential: upstreamA})
	handler, _ := New(f.grants, Policy{Allow: multica.RelayPath, Limit: 1024})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/issues/x", nil)
	req.Header.Set("Authorization", "Bearer "+broken)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "127.0.0.1") || strings.Contains(rec.Body.String(), upstreamA) {
		t.Fatalf("transport error leaked: %s", rec.Body)
	}
}

func TestPathPolicyAndRequestForm(t *testing.T) {
	f := newFixture(t, nil, Policy{})
	a := f.issue(t, "attempt-a", upstreamA)
	for _, path := range []string{"/api/tokens", "/api/tokens/", "/api/daemon/tasks/claim", "/api/cli-token", "/api/auth/login", "/ws", "/uploads/x", "/api/issues/../tokens", "/api//tokens", "/api/issues/%2e%2e/tokens", "/api/issues%2F..%2Ftokens", "/api/./tokens"} {
		req, _ := http.NewRequest(http.MethodGet, f.relay.URL, nil)
		req.URL.Opaque = path
		req.Header.Set("Authorization", "Bearer "+a)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s forwarded: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := f.do(t, http.MethodGet, "/api/issues/x", a, nil, map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}); resp.StatusCode != http.StatusForbidden {
		t.Fatal("upgrade forwarded")
	}
	// Absolute-form request targets cannot choose the upstream.
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.relay.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET "+f.target+"/api/issues/x HTTP/1.1\r\nHost: attacker\r\nAuthorization: Bearer "+a+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusForbidden || f.attacker.Load() != 0 {
		t.Fatalf("absolute-form target accepted: %v", err)
	}
	if len(f.seen) != 0 {
		t.Fatal("denied path reached upstream")
	}
	if resp, _ := f.do(t, http.MethodPost, "/api/issues/x/comments", a, strings.NewReader(strings.Repeat("x", 4096)), nil); resp.StatusCode == 200 {
		t.Fatal("body limit not enforced")
	}
}

func TestStreamsAndRevocationCancelsInflight(t *testing.T) {
	cancelled := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}, Policy{Allow: func(string) bool { return true }, Limit: 1024, Withhold: true})
	a := f.issue(t, "attempt-a", upstreamA)
	req, _ := http.NewRequest(http.MethodPost, f.relay.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+a)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The first event arrives before the upstream finishes: chunks are flushed.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("stream not flushed: %q %v", line, err)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	f.grants.Revoke("attempt-a")
	for _, ch := range []chan struct{}{done, cancelled} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("revocation left stream open")
		}
	}
}
