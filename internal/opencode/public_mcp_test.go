package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func publicMCPFixture(t *testing.T, proxyURL, endpoint string, headers map[string]string) (*MCPRelay, *running, string, *httptest.Server) {
	t.Helper()
	proxy, err := ParseMCPPublicProxy(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMCPRelay()
	r := &running{adapter: &Adapter{MCPPublicProxy: proxy}, task: safeTask(), connections: map[string]Remote{"docs": {Type: "remote", URL: endpoint, headers: headers}}, anonymous: map[string]bool{"docs": true}, public: map[string]bool{"docs": true}, tokens: map[string]identity.AccessToken{}}
	opaque, err := m.issue(r)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := relay.New(m, relay.Policy{Allow: MCPRelayPath, Limit: 1 << 20, Reserved: MCPReservedHeaders})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)
	return m, r, opaque, front
}

func TestPublicMCPUsesDeploymentProxyAndSelectedHeaders(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.String() != "http://mcp.public.invalid/tools" || r.Header.Get("Authorization") != "Bearer user-header-secret" || r.Header.Get("Cookie") != "session=user-cookie-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy did not receive only the selected target and scoped headers")
		}
		_, _ = io.WriteString(w, `{"result":{}}`)
	}))
	defer proxy.Close()
	m, run, opaque, front := publicMCPFixture(t, proxy.URL, "http://mcp.public.invalid/tools", map[string]string{"Authorization": "Bearer user-header-secret", "Cookie": "session=user-cookie-secret"})
	req, _ := http.NewRequest("POST", front.URL+"/docs?target=http://127.0.0.1/other", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+opaque)
	req.Header.Set("X-Api-Key", opaque)
	req.Header.Set("Cookie", "caller-cookie")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("Proxy-Authorization", "Basic caller-proxy")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatal("public MCP did not use the trusted proxy")
	}
	m.revoke(run.task.AttemptKey())
	response = mcpRequest(t, "POST", front.URL+"/docs", opaque)
	response.Body.Close()
	if response.StatusCode != 401 || calls.Load() != 1 {
		t.Fatal("revoked public MCP grant was forwarded")
	}
}

func TestPublicMCPProxyDenialNeverFallsBackToDirect(t *testing.T) {
	var direct atomic.Int32
	private := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { direct.Add(1) }))
	defer private.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Private", "user-header-secret")
		w.Header().Set("WWW-Authenticate", "user-header-secret")
		w.Header().Set("Trailer", "X-Private-Trailer")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, "proxy denied private/DNS-resolved address: user-header-secret")
		w.Header().Set("X-Private-Trailer", "user-header-secret")
	}))
	defer proxy.Close()
	for _, endpoint := range []string{private.URL + "/metadata", "http://private-dns.public.invalid/metadata"} {
		_, _, opaque, front := publicMCPFixture(t, proxy.URL, endpoint, map[string]string{"Authorization": "Bearer user-header-secret"})
		response := mcpRequest(t, "POST", front.URL+"/docs", opaque)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 403 || strings.Contains(string(body), "user-header-secret") || response.Header.Get("X-Private") != "" || response.Header.Get("WWW-Authenticate") != "" || len(response.Trailer) != 0 || direct.Load() != 0 {
			t.Fatal("proxy denial leaked credentials or reached the private endpoint directly")
		}
	}
}

func TestPublicMCPHTTPSProxyDenial(t *testing.T) {
	var direct, connects atomic.Int32
	private := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	private.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			direct.Add(1)
		}
	}
	private.StartTLS()
	defer private.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("HTTPS target did not use CONNECT")
		}
		connects.Add(1)
		http.Error(w, "proxy-secret-echo", 403)
	}))
	defer proxy.Close()
	_, _, opaque, front := publicMCPFixture(t, proxy.URL, private.URL+"/mcp", map[string]string{"Authorization": "Bearer user-header-secret"})
	response := mcpRequest(t, "POST", front.URL+"/docs", opaque)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 502 || strings.Contains(string(body), "proxy-secret-echo") || connects.Load() != 1 || direct.Load() != 0 {
		t.Fatal("HTTPS proxy denial bypassed egress or exposed its diagnostic")
	}
}

func TestPublicMCPUsesOnlyExplicitIdentityAndAlwaysDeploymentProxy(t *testing.T) {
	issuer := &stubIssuer{}
	proxy := &url.URL{Scheme: "http", Host: "egress:3128"}
	a := &Adapter{Issuer: issuer, Status: stubStatus{}, MCPPublicProxy: proxy}
	connections := map[string]Remote{
		"managed":  {URL: "https://eco.example.invalid/mcp", identity: true},
		"unknown":  {URL: "https://eco.example.invalid/mcp"},
		"explicit": {URL: "https://eco.example.invalid/mcp", headers: map[string]string{"Authorization": "Bearer own-key"}},
	}
	anonymous, public, err := a.mcpModes(connections)
	if err != nil || anonymous["managed"] || !public["managed"] || !anonymous["unknown"] || !public["unknown"] || !anonymous["explicit"] || !public["explicit"] {
		t.Fatal("IAM selection or deployment transport changed")
	}
	r := &running{adapter: a, task: safeTask(), connections: connections, anonymous: anonymous, public: public, tokens: map[string]identity.AccessToken{}}
	if err = r.refresh(context.Background()); err != nil || issuer.calls != 1 {
		t.Fatalf("unexpected issuance calls=%d err=%v", issuer.calls, err)
	}
	for name := range connections {
		target, ok := r.mcpUpstream(name)
		if !ok || target.proxy != proxy {
			t.Fatal("IAM bypassed deployment proxy")
		}
	}
	a.Issuer = nil
	if _, _, err = a.mcpModes(connections); err == nil {
		t.Fatal("IAM silently downgraded without issuer")
	}
}

func TestPublicMCPNeedsNoIdentityFileAndKeepsHeaderEnvOutsideAttempt(t *testing.T) {
	task := safeTask()
	task.Agent.MCPConfig = json.RawMessage(`{"mcpServers":{"docs":{"url":"https://mcp.public.invalid/mcp","headers":{"Authorization":"Bearer {env:MCP_KEY}"}}}}`)
	if err := json.Unmarshal([]byte(`{"MCP_KEY":"private-env-secret"}`), &task.Agent.CustomEnv); err != nil {
		t.Fatal(err)
	}
	workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1)}
	a := &Adapter{MCPPublicProxy: &url.URL{Scheme: "http", Host: "egress:3128"}, MCPRelay: NewMCPRelay(), MCPRelayURL: "http://mcp-relay:8095", Workloads: workload, Status: stubStatus{}}
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	env := <-workload.envs
	if env["MCP_KEY"] != "" {
		t.Fatal("header ENV was projected into attempt")
	}
	for _, data := range workload.files {
		if strings.Contains(string(data), "private-env-secret") || strings.Contains(string(data), "{env:MCP_KEY}") {
			t.Fatal("header credential was projected into attempt files")
		}
	}
	if err = run.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPublicMCPProxyConfiguration(t *testing.T) {
	for _, raw := range []string{"", "http://egress:3128", "https://egress:3128/"} {
		if _, err := ParseMCPPublicProxy(raw); err != nil {
			t.Errorf("valid proxy rejected: %s", raw)
		}
	}
	for _, raw := range []string{"http://user:secret@egress:3128", "http://egress:3128/path", "http://egress:3128?", "http://egress:3128?token=x", "http://egress:3128/#x", "socks5://egress:1080", "/relative"} {
		if _, err := ParseMCPPublicProxy(raw); err == nil {
			t.Errorf("unsafe proxy accepted: %s", raw)
		}
	}
}

func TestPublicMCPPreservesOnlyConfiguredQuery(t *testing.T) {
	const endpoint = "http://mcp.public.invalid/mcp?project_ref=chosen&api_key=query-secret&x=1&x=2"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != endpoint {
			t.Error("caller changed the configured MCP query")
		}
		_, _ = io.WriteString(w, `{"result":{}}`)
	}))
	defer proxy.Close()
	_, _, opaque, front := publicMCPFixture(t, proxy.URL, endpoint, nil)
	response := mcpRequest(t, "POST", front.URL+"/docs?project_ref=other&api_key=caller&codemode=false", opaque)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("configured MCP query was not forwarded")
	}
}

func TestPublicMCPRejectsRedirectWithoutExposingQuery(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "http://127.0.0.1/private?token=query-secret")
		w.WriteHeader(302)
		_, _ = io.WriteString(w, "query-secret")
	}))
	defer proxy.Close()
	_, _, opaque, front := publicMCPFixture(t, proxy.URL, "http://mcp.public.invalid/mcp?token=query-secret", nil)
	response := mcpRequest(t, "POST", front.URL+"/docs", opaque)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 502 || calls.Load() != 1 || response.Header.Get("Location") != "" || strings.Contains(string(body), "query-secret") {
		t.Fatal("redirect was followed or disclosed a query credential")
	}
}

func TestPublicMCPAttemptCredentialsStaySeparate(t *testing.T) {
	seen := make(chan string, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Host + " " + r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"result":{}}`)
	}))
	defer proxy.Close()
	m, alice, aliceToken, front := publicMCPFixture(t, proxy.URL, "http://alice.public.invalid/mcp", map[string]string{"Authorization": "Bearer alice-secret"})
	bobTask := safeTask()
	bobTask.ID = "10000000-0000-4000-8000-000000000099"
	bob := &running{adapter: alice.adapter, task: bobTask, connections: map[string]Remote{"docs": {URL: "http://bob.public.invalid/mcp", headers: map[string]string{"Authorization": "Bearer bob-secret"}}}, anonymous: map[string]bool{"docs": true}, public: map[string]bool{"docs": true}, tokens: map[string]identity.AccessToken{}}
	bobToken, err := m.issue(bob)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ token, want string }{{aliceToken, "alice.public.invalid Bearer alice-secret"}, {bobToken, "bob.public.invalid Bearer bob-secret"}} {
		response := mcpRequest(t, "POST", front.URL+"/docs", tc.token)
		response.Body.Close()
		if response.StatusCode != 200 || <-seen != tc.want {
			t.Fatal("attempt borrowed another connection's credentials")
		}
	}
	m.revoke(alice.task.AttemptKey())
	response := mcpRequest(t, "POST", front.URL+"/docs", aliceToken)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("revoked attempt retained a public connection")
	}
	response = mcpRequest(t, "POST", front.URL+"/docs", bobToken)
	response.Body.Close()
	if response.StatusCode != 200 || <-seen != "bob.public.invalid Bearer bob-secret" {
		t.Fatal("unrelated attempt changed after revocation")
	}
}

func TestPrivateMCPHasNoImplicitDirectRoute(t *testing.T) {
	proxy := &url.URL{Scheme: "http", Host: "egress:3128"}
	a := &Adapter{Issuer: &stubIssuer{}, MCPPublicProxy: proxy}
	connections := map[string]Remote{"anonymous": {URL: "http://10.8.0.2/private-mcp"}, "iam": {URL: "http://10.8.0.2/private-mcp", identity: true}}
	anonymous, public, err := a.mcpModes(connections)
	if err != nil || !anonymous["anonymous"] || anonymous["iam"] || !public["anonymous"] || !public["iam"] {
		t.Fatal("private URL changed explicit auth/transport policy")
	}
}
