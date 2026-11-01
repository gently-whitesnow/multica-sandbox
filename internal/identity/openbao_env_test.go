package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func openBaoEnvFixture(t *testing.T, jwt *tokenFixture, entity *string, revoked *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/login":
			var credentials map[string]string
			_ = json.NewDecoder(request.Body).Decode(&credentials)
			if credentials["role_id"] != "env-role" || credentials["secret_id"] != "env-secret" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "temporary-login", "entity_id": *entity}})
		case "/identity":
			if request.Header.Get("X-Vault-Token") != "temporary-login" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			response, err := http.Get(jwt.server.URL + "/token")
			if err != nil {
				t.Error("fixture signer unavailable")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			defer response.Body.Close()
			var token map[string]any
			_ = json.NewDecoder(response.Body).Decode(&token)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": token["access_token"]}})
		case "/revoke":
			if request.Header.Get("X-Vault-Token") != "temporary-login" {
				t.Error("wrong cleanup token")
			}
			revoked.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestOpenBaoAgentEnvBindsAuthenticatedEntity(t *testing.T) {
	jwt := newTokenFixture(t)
	entity := "subject-a"
	var revoked atomic.Int32
	bao := openBaoEnvFixture(t, jwt, &entity, &revoked)
	config := jwt.config(t)
	config.AgentEnv, config.Bindings = true, nil
	config.Issuer.TokenURL = ""
	config.Issuer.Profile, config.Issuer.PrincipalClaim, config.Issuer.Audience = "jwt", "eco_agent", "tools"
	config.TokenSource = TokenSourceConfig{Type: "openbao_approle", LoginURL: bao.URL + "/login", TokenURL: bao.URL + "/identity", RevokeURL: bao.URL + "/revoke"}
	service, err := New(config, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	env := agentEnv{EnvClientID: "env-role", EnvClientSecret: "env-secret"}
	for _, agent := range []string{agentA, agentB} {
		jwt.claims["eco_agent"] = "executor"
		if _, err = service.Acquire(context.Background(), Ref{identityTestServer, workspaceA, agent}, env); err != nil {
			t.Fatal("Multica credentials should select the identity without UUID bindings", err)
		}
	}
	for _, mutate := range []func(){
		func() { entity = "different-entity" },
		func() { entity = "" },
		func() { entity = "invalid\nentity" },
		func() { jwt.claims["sub"] = "different-subject" },
		func() { delete(jwt.claims, "eco_agent") },
		func() { jwt.claims["eco_agent"] = []string{"executor"} },
		func() { jwt.claims["aud"] = "different-recipient" },
	} {
		jwt.reset()
		jwt.claims["eco_agent"], entity = "executor", "subject-a"
		before := revoked.Load()
		mutate()
		if _, err = service.Acquire(context.Background(), ref(config), env); err != ErrDenied {
			t.Fatal("OpenBao identity proof failure accepted")
		}
		if revoked.Load() != before+1 {
			t.Fatal("OpenBao proof failure left login active")
		}
	}
}

func TestJWTAgentEnvRequiresSubjectAttestingSource(t *testing.T) {
	fixture := newTokenFixture(t)
	config := fixture.config(t)
	config.AgentEnv, config.Bindings = true, nil
	config.Issuer.Profile, config.Issuer.PrincipalClaim, config.Issuer.Audience = "jwt", "eco_agent", "tools"
	if _, err := New(config, identityTestServer); err != ErrDenied {
		t.Fatal("generic OAuth exchange cannot attest the ENV principal")
	}
}
