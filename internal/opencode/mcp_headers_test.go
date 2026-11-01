package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func TestMCPHeadersAreScopedValidatedAndBounded(t *testing.T) {
	for _, raw := range []map[string]string{
		{"Host": "other-host"}, {"Connection": "keep-alive"}, {"Proxy-Authorization": "private-value"},
		{"X-Forwarded-For": "127.0.0.1"}, {"Content-Length": "10"},
		{"Bad Header": "private-value"}, {"X-Header": "private-value\r\nInjected: true"},
		{"X-Header": "private-value\x00"}, {"X-Header": strings.Repeat("x", 4097)},
		{"Authorization": "one", "authorization": "two"},
	} {
		if _, err := mcpHeaders(raw); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("unsafe headers accepted or exposed")
		}
	}
	large := map[string]string{}
	for index := 0; index < 33; index++ {
		large[fmt.Sprintf("X-Test-%d", index)] = "value"
	}
	if _, err := mcpHeaders(large); err == nil {
		t.Fatal("unbounded header count accepted")
	}
	if _, err := mcpHeaders(map[string]string{"Authorization": "Bearer user-token", "Cookie": "session=user-cookie", "X-User-Id": "configured-user"}); err != nil {
		t.Fatal("explicit recipient-scoped application authentication was denied")
	}
}

func TestMCPHeaderENVUsesOnlyAgentSecrets(t *testing.T) {
	t.Setenv("TOKEN", "private-controller-token")
	var env multica.SecretEnv
	if json.Unmarshal([]byte(`{"TOKEN":"private-agent-token","ACCOUNT":"account-one"}`), &env) != nil {
		t.Fatal("invalid fixture")
	}
	connections := map[string]Remote{"docs": {Type: "remote", URL: "https://tools.example.invalid/mcp", headers: map[string]string{"Authorization": "Bearer {env:TOKEN}", "X-Account": "{env:ACCOUNT}/{env:ACCOUNT}"}}}
	resolved, err := resolveRemoteHeaders(connections, env)
	if err != nil || resolved["docs"].headers["Authorization"] != "Bearer private-agent-token" || resolved["docs"].headers["X-Account"] != "account-one/account-one" {
		t.Fatalf("agent ENV not resolved: %v", err)
	}
	if strings.Join(resolved["docs"].headerEnv, ",") != "ACCOUNT,TOKEN" || connections["docs"].headers["Authorization"] != "Bearer {env:TOKEN}" {
		t.Fatal("consumed ENV names lost or selected configuration mutated")
	}
	data, _ := json.Marshal(resolved)
	if strings.Contains(string(data)+fmt.Sprintf("%+v %#v", resolved, resolved), "private-") {
		t.Fatal("resolved MCP headers leaked")
	}
	if _, err := resolveRemoteHeaders(connections, multica.SecretEnv{}); err == nil || !strings.Contains(err.Error(), "TOKEN") && !strings.Contains(err.Error(), "ACCOUNT") || strings.Contains(err.Error(), "private-controller-token") {
		t.Fatalf("controller ENV substituted or missing variable not reported: %v", err)
	}
}

func TestMCPHeaderENVRejectsInvalidResolvedValues(t *testing.T) {
	for _, tc := range []struct{ template, value string }{
		{"Bearer {env:TOKEN}", "private-agent-token\r\nInjected: true"},
		{"Bearer {env:TOKEN}", strings.Repeat("x", 4097)},
		{"Bearer {env:}", "private-agent-token"},
		{"Bearer {env:TOKEN", "private-agent-token"},
		{"Bearer {env:TOKEN}", ""},
	} {
		var env multica.SecretEnv
		data, _ := json.Marshal(map[string]string{"TOKEN": tc.value})
		_ = json.Unmarshal(data, &env)
		connections := map[string]Remote{"docs": {Type: "remote", URL: "https://tools.example.invalid/mcp", headers: map[string]string{"Authorization": tc.template}}}
		if _, err := resolveRemoteHeaders(connections, env); err == nil || strings.Contains(err.Error(), "private-agent-token") || strings.Contains(err.Error(), "Injected") {
			t.Fatalf("invalid ENV header did not fail safely: %v", err)
		}
	}
}
