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

type fixture struct {
	grants   *Grants
	relay    *httptest.Server
	attacker atomic.Int32
	target   string
	seen     chan *http.Request
}

func newFixture(t *testing.T, upstream http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{grants: NewGrants(), seen: make(chan *http.Request, 16)}
	attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { f.attacker.Add(1) }))
	t.Cleanup(attacker.Close)
	f.target = attacker.URL
	if upstream == nil {
		upstream = func(w http.ResponseWriter, r *http.Request) {
			f.seen <- r.Clone(r.Context())
			http.SetCookie(w, &http.Cookie{Name: "multica_auth", Value: "session"})
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}
	origin := httptest.NewServer(upstream)
	t.Cleanup(origin.Close)
	handler, err := New(origin.URL, f.grants, multica.RelayPath, 1024)
	if err != nil {
		t.Fatal(err)
	}
	f.relay = httptest.NewServer(handler)
	t.Cleanup(f.relay.Close)
	return f
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

func TestTranslatesCredentialToConfiguredOrigin(t *testing.T) {
	f := newFixture(t, nil)
	a, err := f.grants.Issue("attempt-a", upstreamA)
	if err != nil || !strings.HasPrefix(a, Prefix) || strings.Contains(a, upstreamA) {
		t.Fatalf("unsafe opaque credential: %v", err)
	}
	b, _ := f.grants.Issue("attempt-b", upstreamB)
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

func TestEndedWrongAndUpstreamCredentialsDenied(t *testing.T) {
	f := newFixture(t, nil)
	a, _ := f.grants.Issue("attempt-a", upstreamA)
	b, _ := f.grants.Issue("attempt-b", upstreamB)
	if _, err := f.grants.Issue("attempt-b", upstreamB); err == nil {
		t.Fatal("second grant for one attempt accepted")
	}
	f.grants.Revoke("attempt-a")
	f.grants.Revoke("attempt-a")
	for _, bearer := range []string{a, upstreamA, Prefix + strings.Repeat("0", 64), "", b + "x", strings.ToUpper(b)} {
		if resp, _ := f.do(t, http.MethodGet, "/api/issues/x", bearer, nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("credential %q accepted: %d", bearer, resp.StatusCode)
		}
	}
	if resp, _ := f.do(t, http.MethodGet, "/api/issues/x", b, nil, nil); resp.StatusCode != 200 {
		t.Fatal("revoking one attempt affected another")
	}
	<-f.seen
	if len(f.seen) != 0 {
		t.Fatal("denied request reached upstream")
	}
}

func TestRefusesRedirectsAndWithholdsErrors(t *testing.T) {
	var target atomic.Value
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fail" {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "internal detail")
			return
		}
		http.Redirect(w, r, target.Load().(string)+"/collect", http.StatusTemporaryRedirect)
	})
	target.Store(f.target)
	a, _ := f.grants.Issue("attempt-a", upstreamA)
	resp, body := f.do(t, http.MethodPost, "/api/issues/x", a, strings.NewReader("{}"), nil)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" || strings.Contains(body, f.target) || strings.Contains(body, upstreamA) {
		t.Fatalf("redirect not refused: %d %s %s", resp.StatusCode, resp.Header, body)
	}
	if f.attacker.Load() != 0 {
		t.Fatal("redirect followed")
	}
	// Upstream API errors pass through unchanged; transport errors are generic.
	if resp, body := f.do(t, http.MethodGet, "/api/fail", a, nil, nil); resp.StatusCode != 500 || body != "internal detail" {
		t.Fatal("upstream status not preserved")
	}
	handler, _ := New("http://127.0.0.1:1", f.grants, multica.RelayPath, 1024)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/issues/x", nil)
	req.Header.Set("Authorization", "Bearer "+a)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "127.0.0.1") || strings.Contains(rec.Body.String(), upstreamA) {
		t.Fatalf("transport error leaked: %s", rec.Body)
	}
}

func TestPathPolicyAndRequestForm(t *testing.T) {
	f := newFixture(t, nil)
	a, _ := f.grants.Issue("attempt-a", upstreamA)
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

func TestRevocationCancelsInflight(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	a, _ := f.grants.Issue("attempt-a", upstreamA)
	req, _ := http.NewRequest(http.MethodGet, f.relay.URL+"/api/issues/x", nil)
	req.Header.Set("Authorization", "Bearer "+a)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	f.grants.Revoke("attempt-a")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("revocation left stream open")
	}
}
