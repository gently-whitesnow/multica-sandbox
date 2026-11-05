package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func TestIAMHeaderRequiresExactLiteralAuthorization(t *testing.T) {
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer {iam:access_token} extra"},
		{"Authorization": "Bearer {iam:refresh_token}"},
		{"Authorization": "{iam:access_token}"},
		{"Authorization": "Bearer {IAM:access_token}"},
		{"X-Token": "Bearer {iam:access_token}"},
	} {
		if _, err := selectMCP("docs", mcpConnection{URL: "https://tools.example/mcp", Headers: headers}); err == nil {
			t.Fatal("invalid IAM template accepted")
		}
	}
	connection, err := selectMCP("docs", mcpConnection{URL: "https://tools.example/mcp", Headers: map[string]string{"authorization": IAMAuthorization, "X-Account": "selected"}})
	if err != nil || !connection.identity || connection.headers["Authorization"] != "" || connection.headers["X-Account"] != "selected" {
		t.Fatal("literal IAM not consumed alongside ordinary headers")
	}
}

func TestHeaderEnvironmentCannotRequestIAMOrRevealBootstrapCredentials(t *testing.T) {
	for _, tc := range []struct{ template, value string }{
		{"Bearer {env:TOKEN}", IAMAuthorization},
		{"{env:IAM_CLIENT_ID}", "bootstrap-client"},
		{"{env:IAM_CLIENT_SECRET}", "bootstrap-secret"},
	} {
		var env multica.SecretEnv
		_ = json.Unmarshal([]byte(`{"TOKEN":"Bearer {iam:access_token}","IAM_CLIENT_ID":"bootstrap-client","IAM_CLIENT_SECRET":"bootstrap-secret"}`), &env)
		connection := map[string]Remote{"docs": {URL: "https://tools.example/mcp", headers: map[string]string{"Authorization": tc.template}}}
		if _, err := resolveRemoteHeaders(connection, env); err == nil || strings.Contains(err.Error(), tc.value) && tc.value != "Bearer {iam:access_token}" {
			t.Fatal("header ENV selected IAM or exposed its bootstrap credential")
		}
	}
}

func TestIAMRelayUsesRotatingTokenAndAdditionalHeadersThroughProxy(t *testing.T) {
	seen := make(chan string, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.String() + " " + r.Header.Get("Authorization") + " " + r.Header.Get("X-Account")
		_, _ = io.WriteString(w, `{"result":{}}`)
	}))
	defer proxy.Close()
	proxyURL, _ := ParseMCPPublicProxy(proxy.URL)
	connection, err := selectMCP("docs", mcpConnection{URL: "http://private.invalid/mcp?tenant=chosen", Headers: map[string]string{"Authorization": IAMAuthorization, "X-Account": "selected"}})
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Issuer: &stubIssuer{}, MCPPublicProxy: proxyURL}
	connections := map[string]Remote{"docs": connection}
	anonymous, public, err := a.mcpModes(connections)
	if err != nil {
		t.Fatal(err)
	}
	r := &running{adapter: a, task: safeTask(), connections: connections, anonymous: anonymous, public: public, tokens: map[string]identity.AccessToken{}}
	m := NewMCPRelay()
	opaque, err := m.issue(r)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := relay.New(m, relay.Policy{Allow: MCPRelayPath, Limit: 1 << 20, Reserved: MCPReservedHeaders})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(handler)
	defer front.Close()
	for _, value := range []string{"jwt-one", "jwt-two"} {
		r.mu.Lock()
		r.tokens["docs"] = identity.NewAccessToken(value, time.Now().Add(time.Minute))
		r.mu.Unlock()
		response := mcpRequest(t, "POST", front.URL+"/docs?tenant=caller", opaque)
		response.Body.Close()
		if response.StatusCode != 200 || <-seen != "http://private.invalid/mcp?tenant=chosen Bearer "+value+" selected" {
			t.Fatal("IAM/additional headers/query or deployment proxy changed")
		}
	}
	r.mu.Lock()
	r.tokens["docs"] = identity.NewAccessToken("expired", time.Now())
	r.mu.Unlock()
	response := mcpRequest(t, "POST", front.URL+"/docs", opaque)
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || len(seen) != 0 {
		t.Fatal("expired IAM fell back to anonymous/static authorization")
	}
}
