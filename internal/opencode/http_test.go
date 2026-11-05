package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

func httpServiceFixture(t *testing.T) (*MCPRelay, *running, string, *mcpUpstream, string) {
	m, r, opaque, upstream, front := mcpRelayFixture(t)
	origin := strings.TrimSuffix(r.connections["docs"].URL, "/mcp")
	r.httpServices = map[string]identity.HTTPService{"control": {Name: "control", Origin: origin, Routes: []identity.HTTPRoute{{Method: "GET", Path: "/api/v1/settings"}, {Method: "POST", PathPrefix: "/api/v1/deployment-access/"}}}}
	r.httpTokens = map[string]identity.AccessToken{"control": identity.NewAccessToken("jwt-http-one", time.Now().Add(time.Minute))}
	r.adapter = &Adapter{MCPRelayURL: front.URL}
	r.mcpToken = opaque
	return m, r, opaque, upstream, front.URL
}

func TestHTTPServiceRelayRoutesAndRotation(t *testing.T) {
	_, r, opaque, upstream, front := httpServiceFixture(t)
	path := "/http/control/api/v1/deployment-access/fixture/credentials"
	response := mcpRequest(t, "POST", front+path+"?audit=fixture", opaque)
	response.Body.Close()
	if response.StatusCode != 200 || upstream.last() != "POST /api/v1/deployment-access/fixture/credentials?audit=fixture Bearer jwt-http-one mcp-session" {
		t.Fatal("HTTP relay changed route or used caller credential")
	}
	r.mu.Lock()
	r.httpTokens["control"] = identity.NewAccessToken("jwt-http-two", time.Now().Add(time.Minute))
	r.mu.Unlock()
	response = mcpRequest(t, "POST", front+path, opaque)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(upstream.last(), "Bearer jwt-http-two") {
		t.Fatal("HTTP relay did not rotate JWT")
	}
	env := r.environment()
	if env["SANDBOX_HTTP_CONTROL_URL"] != front+"/http/control" || env["SANDBOX_HTTP_CONTROL_TOKEN"] != opaque {
		t.Fatal("HTTP helper connection missing")
	}
	for _, value := range env {
		if strings.Contains(value, "jwt-http") {
			t.Fatal("JWT entered attempt env")
		}
	}
}

func TestHTTPServiceRelayDeniesForeignPathsMethodsAndCredentials(t *testing.T) {
	m, r, opaque, upstream, front := httpServiceFixture(t)
	for _, check := range []struct {
		method, path, token string
		status              int
	}{
		{"POST", "/http/control/api/v1/settings", opaque, http.StatusForbidden},
		{"POST", "/http/control/api/v1/agents", opaque, http.StatusForbidden},
		{"POST", "/http/other/api/v1/deployment-access/fixture/credentials", opaque, http.StatusForbidden},
		{"POST", "/http/control/api/v1/deployment-access/../agents", opaque, http.StatusForbidden},
		{"POST", "/http/control/api/v1/deployment-access/%2fagents", opaque, http.StatusForbidden},
		{"POST", "/http/control/api/v1/deployment-access/fixture/credentials", "jwt-http-one", http.StatusUnauthorized},
	} {
		response := mcpRequest(t, check.method, front+check.path, check.token)
		response.Body.Close()
		if response.StatusCode != check.status {
			t.Errorf("%s %s: %d, want %d", check.method, check.path, response.StatusCode, check.status)
		}
	}
	if len(upstream.seen) != 0 {
		t.Fatal("rejected HTTP request reached recipient")
	}
	r.mu.Lock()
	r.httpTokens["control"] = identity.NewAccessToken("expired", time.Now())
	r.mu.Unlock()
	response := mcpRequest(t, "POST", front+"/http/control/api/v1/deployment-access/fixture/credentials", opaque)
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("expired HTTP JWT: %d", response.StatusCode)
	}
	m.revoke(r.task.AttemptKey())
	response = mcpRequest(t, "POST", front+"/http/control/api/v1/deployment-access/fixture/credentials", opaque)
	response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatal("ended attempt reached HTTP service")
	}
}

func TestHTTPServiceRevocationCancelsOpenStream(t *testing.T) {
	m, r, opaque, upstream, front := httpServiceFixture(t)
	response := mcpRequest(t, http.MethodGet, front+"/http/control/api/v1/settings", opaque)
	defer response.Body.Close()
	select {
	case <-upstream.streams:
	case <-time.After(time.Second):
		t.Fatal("HTTP stream not opened")
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, response.Body); close(done) }()
	m.revoke(r.task.AttemptKey())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ended HTTP stream retained access")
	}
}

type httpTestIssuer struct {
	stubIssuer
	services []identity.HTTPService
	calls    int
}

func (i *httpTestIssuer) HTTPServicesFor(_ identity.Ref, env identity.Env) []identity.HTTPService {
	i.env = env
	return i.services
}
func (i *httpTestIssuer) AcquireForHTTP(context.Context, identity.Ref, identity.Env, string) (identity.AccessToken, error) {
	i.calls++
	return identity.NewAccessToken("http-jwt", time.Now().Add(time.Minute)), nil
}

func TestHTTPIdentityWithoutSelectedMCP(t *testing.T) {
	issuer := &httpTestIssuer{services: []identity.HTTPService{{Name: "control", Origin: "https://control.example", Routes: []identity.HTTPRoute{{Method: "GET", Path: "/api/v1/settings"}}}}}
	r := &running{adapter: &Adapter{Issuer: issuer, MCPRelay: NewMCPRelay(), MCPRelayURL: "http://relay.example"}, task: safeTask()}
	if err := r.configureHTTP(); err != nil {
		t.Fatal(err)
	}
	if issuer.env == nil {
		t.Fatal("HTTP selection did not receive claim environment")
	}
	if err := r.refreshHTTP(context.Background(), identity.Ref{}); err != nil || issuer.calls != 1 {
		t.Fatal("HTTP-only identity not acquired", err)
	}
	if err := r.refreshHTTP(context.Background(), identity.Ref{}); err != nil || issuer.calls != 1 {
		t.Fatal("fresh HTTP token unnecessarily reissued", err)
	}
	if validProcessEnvName("SANDBOX_HTTP_CONTROL_TOKEN") {
		t.Fatal("caller can override relay credential")
	}
}

func TestHTTPServiceBasePathAndTrustedHeaders(t *testing.T) {
	m, r, opaque, upstream, front := httpServiceFixture(t)
	service := r.httpServices["control"]
	service.Origin += "/external/agent"
	service.Headers = map[string]string{"X-Client": "trusted"}
	r.httpServices["control"] = service
	response := mcpRequest(t, "POST", front+"/http/control/api/v1/deployment-access/fixture/credentials", opaque)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.HasPrefix(upstream.last(), "POST /external/agent/api/") {
		t.Fatal("service base path lost")
	}
	request, _ := http.NewRequest("POST", front+"/http/control/api/v1/deployment-access/fixture/credentials", nil)
	request.Header.Set("Authorization", "Bearer "+opaque)
	request.Header.Set("X-Client", "spoofed")
	lease, ok := m.Authorize(request)
	if !ok {
		t.Fatal("HTTP grant not found")
	}
	defer lease.Release()
	if lease.Header.Get("X-Client") != "trusted" {
		t.Fatal("caller replaced trusted header")
	}
}

func TestHTTPStaticCredentialAndAttributionStayOutsideAttempt(t *testing.T) {
	received := make(chan [3]string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- [3]string{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Client")}
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	_, r, opaque, _, front := httpServiceFixture(t)
	r.httpServices["control"] = identity.HTTPService{Name: "control", Origin: upstream.URL + "/external/agent", Headers: map[string]string{"X-Client": "fred"}, Routes: []identity.HTTPRoute{{Method: "GET", PathPrefix: "/api/"}}}
	r.httpTokens["control"] = identity.NewAccessToken("service-credential-only", time.Now().Add(30*time.Second))
	request, _ := http.NewRequest("GET", front+"/http/control/api/openapi/index", nil)
	request.Header.Set("Authorization", "Bearer "+opaque)
	request.Header.Set("X-Client", "spoofed")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("static service request refused")
	}
	got := <-received
	if got != [3]string{"/external/agent/api/openapi/index", "Bearer service-credential-only", "fred"} {
		t.Fatal("service path, credential or trusted attribution changed")
	}
	for _, value := range r.environment() {
		if strings.Contains(value, "service-credential-only") {
			t.Fatal("service credential entered attempt environment")
		}
	}
}

// A globally declared JWT helper must not make IAM mandatory for ordinary agents.
func TestOrdinaryAgentWithoutIAMDoesNotRequireHTTPIdentity(t *testing.T) {
	config := identity.Config{Version: 1, AgentEnv: true,
		Issuer:       identity.IssuerConfig{Profile: "jwt", URL: "https://bao.example/v1/identity/oidc", JWKSURL: "https://bao.example/v1/identity/oidc/.well-known/keys", Audience: "tools", PrincipalClaim: "eco_agent"},
		TokenSource:  identity.TokenSourceConfig{Type: "openbao_approle", LoginURL: "https://bao.example/v1/auth/approle/login", TokenURL: "https://bao.example/v1/identity/oidc/token/tools", RevokeURL: "https://bao.example/v1/auth/token/revoke-self"},
		HTTPServices: []identity.HTTPService{{Name: "control", Origin: "https://control.example", Routes: []identity.HTTPRoute{{Method: "GET", Path: "/api/v1/settings"}}}},
	}
	service, err := identity.New(config, "https://multica.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	for _, partial := range []bool{false, true} {
		task := safeTask()
		task.Agent.MCPConfig = json.RawMessage(`{"mcpServers":{"public":{"url":"https://public.example/mcp"}}}`)
		if partial {
			_ = json.Unmarshal([]byte(`{"IAM_CLIENT_ID":"incomplete-pair"}`), &task.Agent.CustomEnv)
		}
		workload := &stubWorkload{envs: make(chan map[string]string, 1)}
		adapter := &Adapter{Server: "https://multica.example.invalid", Issuer: service, MCPRelay: NewMCPRelay(), MCPRelayURL: "http://relay.example", Workloads: workload, Status: stubStatus{}}
		run, err := adapter.Start(context.Background(), task)
		if partial {
			if err == nil || workload.writes != 0 {
				t.Fatal("partial IAM pair silently downgraded to ordinary access")
			}
			continue
		}
		if err != nil {
			t.Fatal("ordinary agent required IAM credentials", err)
		}
		env := <-workload.envs
		if env["SANDBOX_HTTP_CONTROL_URL"] != "" || env["SANDBOX_HTTP_CONTROL_TOKEN"] != "" {
			t.Fatal("missing identity still projected its HTTP helper")
		}
		if err = run.Remove(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
