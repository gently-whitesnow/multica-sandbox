package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func secret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
func session(c configuration) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims, _ := json.Marshal(map[string]any{"sub": user, "iat": time.Now().Unix(), "exp": time.Now().Add(24 * time.Hour).Unix()})
	message := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(claims)
	mac := hmac.New(sha256.New, []byte(c.JWT))
	mac.Write([]byte(message))
	return message + "." + enc(mac.Sum(nil))
}
func initialize() error {
	if _, err := os.Stat("/config/control.json"); err == nil {
		c, err := load()
		if err != nil {
			return err
		}
		return writeLLMEnv(c)
	}
	c := configuration{secret(), secret(), secret(), secret(), secret()[:12]}
	roles := []any{map[string]any{"name": "fixture-reader"}, map[string]any{"name": "inference-demo"}}
	scopes := []any{}
	for _, name := range []string{"mcp", "inference"} {
		scopes = append(scopes, map[string]any{"name": name, "protocol": "openid-connect", "attributes": map[string]string{"include.in.token.scope": "true"}, "protocolMappers": []any{map[string]any{"name": "audience", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper", "config": map[string]string{"included.custom.audience": "sandbox-" + name, "access.token.claim": "true", "id.token.claim": "false"}}}})
	}
	realm := map[string]any{"realm": "e2e", "enabled": true, "accessTokenLifespan": 180, "roles": map[string]any{"realm": roles}, "clientScopes": scopes, "clients": []any{
		map[string]any{"clientId": "agent-demo", "secret": c.Client, "enabled": true, "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false, "defaultClientScopes": []string{"roles"}, "optionalClientScopes": []string{"mcp", "inference"}, "fullScopeAllowed": true, "protocolMappers": []any{map[string]any{"name": "agent-roles", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-realm-role-mapper", "config": map[string]string{"multivalued": "true", "claim.name": "realm_access.roles", "jsonType.label": "String", "access.token.claim": "true", "id.token.claim": "false"}}}},
	}, "users": []any{map[string]any{"id": agentID, "username": "service-account-agent-demo", "enabled": true, "serviceAccountClientId": "agent-demo", "realmRoles": []string{"fixture-reader", "inference-demo"}}}}
	if err := writeJSON("/realm/realm.json", realm); err != nil {
		return err
	}
	if err := os.Chmod("/realm/realm.json", 0644); err != nil {
		return err
	}
	proxy := fmt.Sprintf("config-version: 8\nserver:\n  host: ''\n  port: 8317\nmanagement:\n  secret-key: ''\n  disable-control-panel: true\naccess:\n  api-keys: ['%s']\noauth:\n  auth-dir: /auth\nobservability:\n  logs:\n    debug: false\n    request-log: false\n", c.Proxy)
	if err := os.WriteFile("/config/cliproxy.yaml", []byte(proxy), 0600); err != nil {
		return err
	}
	env := fmt.Sprintf("JWT_SECRET=%s\nDATABASE_URL=postgres://fixture:fixture@postgres:5432/fixture?sslmode=disable\n", c.JWT)
	if err := os.WriteFile("/config/multica.env", []byte(env), 0600); err != nil {
		return err
	}
	if err := writeLLMEnv(c); err != nil {
		return err
	}
	return writeJSON("/config/control.json", c)
}

func writeLLMEnv(c configuration) error {
	return os.WriteFile("/config/litellm.env", []byte("PROXY_BRIDGE_KEY="+c.Proxy+"\nLITELLM_MASTER_KEY=sk-"+c.Master+"\n"), 0600)
}
