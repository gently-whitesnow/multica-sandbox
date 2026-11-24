package opencode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const attemptTestServer = "https://multica.example.invalid/multica"
const attemptTestKey = "test-only-attempt-signing-secret-1234"

func attemptContextConfig(t *testing.T, url string) MCPAttemptContextConfig {
	t.Helper()
	file := filepath.Join(t.TempDir(), "attempt-key")
	if err := os.WriteFile(file, []byte(" "+attemptTestKey+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return MCPAttemptContextConfig{URL: url, Audience: "idp-postgres", KeyFile: file}
}

type attemptContextRelay struct {
	seen     chan http.Header
	front    *httptest.Server
	upstream string
}

func newAttemptContextFixture(t *testing.T, enabled, restrictIAM bool) (*attemptContextRelay, *MCPRelay) {
	t.Helper()
	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		headers := r.Header.Clone()
		for name, values := range r.Trailer {
			headers[http.TrailerPrefix+name] = values
		}
		seen <- headers
		w.Header().Set("X-Idp-Attempt-Context", "reflected-header")
		w.Header().Set("Mcp-Session-Id", "upstream-session")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	m := NewMCPRelay()
	if enabled {
		var err error
		config := attemptContextConfig(t, upstream.URL+"/mcp")
		config.RestrictIAMToTarget = restrictIAM
		m, err = NewMCPRelayWithAttemptContext(config, attemptTestServer)
		if err != nil {
			t.Fatal(err)
		}
	}
	handler, err := relay.New(m, relay.Policy{Allow: MCPRelayPath, Limit: 1 << 20, Reserved: MCPReservedHeaders})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)
	return &attemptContextRelay{seen: seen, front: front, upstream: upstream.URL}, m
}

func (f *attemptContextRelay) request(t *testing.T, route, opaque string) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.front.URL+route, strings.NewReader("test request"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+opaque)
	req.Header["x-IDP-attempt-context"] = []string{"forged-one", "forged-two"}
	req.Header.Set("Mcp-Session-Id", "incoming-session")
	req.ContentLength = -1
	req.Trailer = http.Header{"X-Idp-Attempt-Context": {"forged-trailer"}}
	res, err := f.front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Idp-Attempt-Context") != "" || res.Header.Get("Mcp-Session-Id") != "upstream-session" {
		t.Fatalf("unexpected upstream status or response headers: %d", res.StatusCode)
	}
	headers := <-f.seen
	_, attemptTrailer := headers[http.TrailerPrefix+"X-Idp-Attempt-Context"]
	if headers.Get("Mcp-Session-Id") != "incoming-session" || attemptTrailer {
		t.Fatal("MCP session header changed or caller attempt trailer escaped")
	}
	return headers
}

func attemptContextRun(t *testing.T, url, bearer string, managed bool) *running {
	t.Helper()
	auth := IAMAuthorization
	if !managed {
		auth = "Bearer " + bearer
	}
	connection, err := selectMCP("tools", mcpConnection{URL: url, Headers: map[string]string{
		"Authorization": auth, "x-idp-aTTEMPT-contexT": "forged-static {iam:something}",
		"X-Account": "unchanged",
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := &running{task: safeTask(), confirmed: time.Now(), connections: map[string]Remote{"tools": connection}}
	r.anonymous = map[string]bool{"tools": !managed}
	r.tokens = map[string]identity.AccessToken{"tools": identity.NewAccessToken(bearer, time.Now().Add(time.Minute))}
	return r
}

func TestMCPAttemptContextBindsConfirmedClaimToCurrentBearer(t *testing.T) {
	f, m := newAttemptContextFixture(t, true, true)
	r := attemptContextRun(t, f.upstream+"/mcp", "test-jwt-one", true)
	opaque, err := m.issue(r)
	if err != nil {
		t.Fatal(err)
	}
	check := func(token string) {
		t.Helper()
		headers := f.request(t, "/tools?unused", opaque)
		if headers.Get("Authorization") != "Bearer "+token || headers.Get("X-Account") != "unchanged" {
			t.Fatal("MCP authorization or ordinary headers changed")
		}
		values := headers.Values("X-Idp-Attempt-Context")
		if len(values) != 1 {
			t.Fatal("trusted MCP must receive one signed context")
		}
		payload := verifyAttemptContext(t, values[0])
		want := map[string]any{
			"v": float64(1), "aud": "idp-postgres", "source": "multica-sandbox", "server": attemptTestServer,
			"workspaceId": r.task.WorkspaceID, "agentId": r.task.AgentID, "taskId": r.task.ID,
			"attempt": r.task.AttemptKey(), "tokenHash": fmt.Sprintf("%x", sha256.Sum256([]byte(token))),
		}
		for name, value := range want {
			if payload[name] != value {
				t.Errorf("incorrect %s", name)
			}
		}
		issued, ok := payload["iat"].(float64)
		if !ok || issued != float64(int64(issued)) || int64(issued) < time.Now().Unix()-10 || int64(issued) > time.Now().Unix() || len(payload) != len(want)+1 {
			t.Fatal("incorrect issued time or extra payload fields")
		}
	}
	check("test-jwt-one")
	r.mu.Lock()
	r.tokens["tools"] = identity.NewAccessToken("test-jwt-two", time.Now().Add(time.Minute))
	r.mu.Unlock()
	check("test-jwt-two")
}

func verifyAttemptContext(t *testing.T, envelope string) map[string]any {
	t.Helper()
	payload, signature, hasSignature := strings.Cut(envelope, ".")
	raw, payloadErr := base64.RawURLEncoding.Strict().DecodeString(payload)
	mac, signatureErr := base64.RawURLEncoding.Strict().DecodeString(signature)
	expected := hmac.New(sha256.New, []byte(attemptTestKey))
	_, _ = expected.Write([]byte(payload))
	if !hasSignature || payloadErr != nil || signatureErr != nil || !hmac.Equal(mac, expected.Sum(nil)) {
		t.Fatal("invalid attempt envelope signature or encoding")
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal("attempt envelope must carry UTF-8 JSON")
	}
	return fields
}

func TestMCPAttemptContextStripsUntrustedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, urlSuffix  string
		enabled, managed bool
		confirmed        time.Time
		ended            bool
	}{
		{name: "disabled", urlSuffix: "/mcp", managed: true, confirmed: time.Now()},
		{name: "different path", urlSuffix: "/mcp/other", enabled: true, managed: true, confirmed: time.Now()},
		{name: "different query", urlSuffix: "/mcp?tenant=other", enabled: true, managed: true, confirmed: time.Now()},
		{name: "static bearer", urlSuffix: "/mcp", enabled: true, confirmed: time.Now()},
		{name: "unconfirmed", urlSuffix: "/mcp", enabled: true, managed: true},
		{name: "stale confirmation", urlSuffix: "/mcp", enabled: true, managed: true, confirmed: time.Now().Add(-time.Minute)},
		{name: "ended", urlSuffix: "/mcp", enabled: true, managed: true, confirmed: time.Now(), ended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, m := newAttemptContextFixture(t, tc.enabled, false)
			r := attemptContextRun(t, f.upstream+tc.urlSuffix, "test-jwt", tc.managed)
			r.confirmed, r.ended = tc.confirmed, tc.ended
			opaque, err := m.issue(r)
			if err != nil {
				t.Fatal(err)
			}
			headers := f.request(t, "/tools", opaque)
			if headers.Get("Authorization") != "Bearer test-jwt" || headers.Get("X-Account") != "unchanged" || len(headers.Values("X-Idp-Attempt-Context")) != 0 {
				t.Fatal("untrusted context escaped or ordinary MCP behavior changed")
			}
		})
	}
}

func TestMCPAttemptContextDoesNotUseHTTPServiceIdentity(t *testing.T) {
	f, m := newAttemptContextFixture(t, true, true)
	r := attemptContextRun(t, f.upstream+"/mcp", "test-mcp-jwt", true)
	r.httpServices = map[string]identity.HTTPService{"control": {
		Name: "control", Origin: f.upstream,
		Routes:  []identity.HTTPRoute{{Method: http.MethodPost, Path: "/mcp"}},
		Headers: map[string]string{"X-Idp-Attempt-Context": "injected-http-context"},
	}}
	r.httpTokens = map[string]identity.AccessToken{"control": identity.NewAccessToken("test-http-jwt", time.Now().Add(time.Minute))}
	opaque, err := m.issue(r)
	if err != nil {
		t.Fatal(err)
	}
	headers := f.request(t, "/http/control/mcp", opaque)
	if headers.Get("Authorization") != "Bearer test-http-jwt" || len(headers.Values("X-Idp-Attempt-Context")) != 0 {
		t.Fatal("HTTP service attribution or credential changed")
	}
}

func TestMCPAttemptContextRestrictsIAMToExactTarget(t *testing.T) {
	f, m := newAttemptContextFixture(t, true, true)
	r := attemptContextRun(t, f.upstream+"/mcp?different", "test-jwt", true)
	ordinary, err := selectMCP("ordinary", mcpConnection{URL: f.upstream + "/ordinary"})
	if err != nil {
		t.Fatal(err)
	}
	r.connections["ordinary"], r.anonymous["ordinary"] = ordinary, true
	opaque, err := m.issue(r)
	if err != nil {
		t.Fatal(err)
	}
	res := mcpRequest(t, http.MethodPost, f.front.URL+"/tools", opaque)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || len(f.seen) != 0 {
		t.Fatal("nonmatching IAM destination received a request")
	}
	headers := f.request(t, "/ordinary", opaque)
	if headers.Get("Authorization") != "" || len(headers.Values("X-Idp-Attempt-Context")) != 0 {
		t.Fatal("ordinary MCP received IAM or attempt context")
	}
}

func TestMCPAttemptContextRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, url, audience, key string }{
		{"relative URL", "/mcp", "idp-postgres", attemptTestKey},
		{"credentials in URL", "https://user:pass@example.invalid/mcp", "idp-postgres", attemptTestKey},
		{"empty audience", "https://example.invalid/mcp", "", attemptTestKey},
		{"short key", "https://example.invalid/mcp", "idp-postgres", "short-test-key"},
		{"Unicode key", "https://example.invalid/mcp", "idp-postgres", attemptTestKey + "ю"},
		{"oversized key", "https://example.invalid/mcp", "idp-postgres", strings.Repeat("a", 4097)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := attemptContextConfig(t, tc.url)
			config.Audience = tc.audience
			if err := os.WriteFile(config.KeyFile, []byte(tc.key), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := NewMCPRelayWithAttemptContext(config, attemptTestServer)
			if m != nil || err == nil || strings.Contains(err.Error(), tc.key) {
				t.Fatal("malformed configuration accepted or key exposed in validation error")
			}
		})
	}
}
