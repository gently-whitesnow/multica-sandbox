package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestDisposableOpenBao runs only against the loopback dev container created by
// scripts/test-openbao-identity.sh. No production address/credential is accepted.
func TestDisposableOpenBao(t *testing.T) {
	address := os.Getenv("SANDBOX_TEST_BAO_URL")
	if address == "" {
		t.Skip("disposable OpenBao fixture only")
	}
	if len(address) < 17 || address[:17] != "http://127.0.0.1:" {
		t.Fatal("fixture must be loopback")
	}
	rootData, err := os.ReadFile(os.Getenv("SANDBOX_TEST_BAO_ROOT_FILE"))
	if err != nil {
		t.Fatal("fixture root unavailable")
	}
	root := string(rootData)
	api := func(method, path string, input any) map[string]any {
		t.Helper()
		data, _ := json.Marshal(input)
		req, e := http.NewRequest(method, address+"/v1/"+path, bytes.NewReader(data))
		if e != nil {
			t.Fatal("fixture request construction")
		}
		req.Header.Set("X-Vault-Token", root)
		response, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal("fixture request failed")
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("fixture setup %s: HTTP %d", path, response.StatusCode)
		}
		result := map[string]any{}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 65536))
		if len(body) > 0 && json.Unmarshal(body, &result) != nil {
			t.Fatal("fixture setup invalid response")
		}
		return result
	}
	api("POST", "sys/auth/approle", map[string]any{"type": "approle"})
	api("POST", "identity/oidc/config", map[string]any{"issuer": address})
	api("POST", "identity/oidc/key/fixture", map[string]any{"allowed_client_ids": []string{"*"}, "algorithm": "RS256"})
	api("POST", "identity/oidc/role/fixture", map[string]any{"key": "fixture", "ttl": "3s", "template": `{"eco_agent":"fixture-agent"}`})
	role := api("GET", "identity/oidc/role/fixture", nil)["data"].(map[string]any)
	api("POST", "sys/policies/acl/fixture", map[string]any{"policy": `path "identity/oidc/token/fixture" { capabilities=["read"] }
path "auth/token/revoke-self" { capabilities=["update"] }`})
	api("POST", "auth/approle/role/fixture", map[string]any{"token_policies": []string{"fixture"}, "token_ttl": "30s", "token_no_default_policy": true})
	roleID := api("GET", "auth/approle/role/fixture/role-id", nil)["data"].(map[string]any)["role_id"].(string)
	c := config(t)
	c.Issuer = IssuerConfig{URL: address + "/v1/identity/oidc", JWKSURL: address + "/v1/identity/oidc/.well-known/keys", Profile: "jwt", Audience: role["client_id"].(string), PrincipalClaim: "eco_agent", MaxTTLSeconds: 3}
	c.TokenSource = TokenSourceConfig{Type: "openbao_approle", LoginURL: address + "/v1/auth/approle/login", TokenURL: address + "/v1/identity/oidc/token/fixture", RevokeURL: address + "/v1/auth/token/revoke-self"}
	c.Bindings[0].ClientID, c.Bindings[0].Principal = roleID, "fixture-agent"
	service, e := New(c, identityTestServer)
	if e != nil {
		t.Fatal("fixture service config", e)
	}
	service.SetDebug(t.Logf)
	var previous AccessToken
	for range 2 {
		if !previous.ExpiresAt.IsZero() {
			time.Sleep(time.Until(previous.ExpiresAt) + 100*time.Millisecond)
		}
		secret := api("POST", "auth/approle/role/fixture/secret-id", nil)["data"].(map[string]any)["secret_id"].(string)
		if e = os.WriteFile(c.Bindings[0].SecretFile, []byte(secret), 0600); e != nil {
			t.Fatal(e)
		}
		token, e := service.Acquire(context.Background(), ref(c), nil)
		if e != nil || token.Bearer() == "" {
			t.Fatal("real OpenBao identity issuance", e)
		}
		if previous.Bearer() == token.Bearer() {
			t.Fatal("rotation did not produce a fresh JWT")
		}
		previous = token
	}
	c.Bindings[0].Principal = "other-agent"
	wrong, _ := New(c, identityTestServer)
	if _, e = wrong.Acquire(context.Background(), ref(c), nil); e != ErrDenied {
		t.Fatal("wrong real principal accepted")
	}
	c.Bindings[0].Principal = "fixture-agent"
	c.Issuer.Audience = "other-recipient"
	wrong, _ = New(c, identityTestServer)
	if _, e = wrong.Acquire(context.Background(), ref(c), nil); e != ErrDenied {
		t.Fatal("wrong real audience accepted")
	}
	if e = os.WriteFile(c.Bindings[0].SecretFile, []byte("revoked-or-wrong"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = service.Acquire(context.Background(), ref(c), nil); e != ErrDenied {
		t.Fatal("wrong real credential accepted")
	}
	if !previous.ExpiresAt.After(time.Now()) {
		t.Fatal("token already expired")
	}
	fmt.Println("PASS disposable OpenBao AppRole → identity JWT; file rotation, principal/audience/credential denial")
}
