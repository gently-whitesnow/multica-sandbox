package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const fixtureServer = "https://multica.example.invalid"

const serviceSubject = "30000000-0000-4000-8000-000000000001"

func resolverConfig() identity.Config {
	return identity.Config{Version: 1, AllowHTTP: true, Issuer: identity.IssuerConfig{URL: issuer, TokenURL: issuer + "/protocol/openid-connect/token", JWKSURL: issuer + "/protocol/openid-connect/certs", MaxTTLSeconds: 180}, MCP: []string{"http://gateway:8080/mcp"}, Bindings: []identity.Binding{{WorkspaceID: "10000000-0000-4000-8000-000000000001", AgentID: "20000000-0000-4000-8000-000000000001", ClientID: "example-agent", SecretFile: "/secrets/client"}}}
}

// Every credential source must obtain and verify a real Keycloak token of the client.
func checkResolvers(ctx context.Context) error {
	c := resolverConfig()
	ref := identity.Ref{Server: fixtureServer, WorkspaceID: c.Bindings[0].WorkspaceID, AgentID: c.Bindings[0].AgentID}
	static, err := identity.New(c, fixtureServer)
	if err != nil {
		return err
	}
	if _, err = static.AcquireForMCP(ctx, ref, nil, "http://gateway:8080/mcp"); err != nil {
		return fmt.Errorf("static identity issuance failed")
	}
	secret, err := os.ReadFile("/secrets/client")
	if err != nil {
		return fmt.Errorf("fixture credential unavailable")
	}
	admin, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return fmt.Errorf("fixture resolver authentication unavailable")
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Version int          `json:"version"`
			Agent   identity.Ref `json:"agent"`
		}
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+string(admin) || json.NewDecoder(r.Body).Decode(&request) != nil || request.Version != 1 || request.Agent != ref {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "agent": ref, "client_id": "example-agent", "client_secret": string(secret)})
	}))
	defer remote.Close()
	c.Bindings = nil
	c.External = &identity.ExternalConfig{URL: remote.URL, BearerFile: "/secrets/admin"}
	external, err := identity.New(c, fixtureServer)
	if err != nil {
		return err
	}
	if _, err = external.AcquireForMCP(ctx, ref, nil, "http://gateway:8080/mcp"); err != nil {
		return fmt.Errorf("external identity issuance failed")
	}
	if _, err = external.AcquireForMCP(ctx, ref, nil, "http://unapproved:8080/mcp"); err != identity.ErrDenied {
		return fmt.Errorf("unapproved MCP delivery accepted")
	}
	envConfig := resolverConfig()
	envConfig.Bindings, envConfig.AgentEnv = nil, true
	fromEnv, err := identity.New(envConfig, fixtureServer)
	if err != nil {
		return err
	}
	var env multica.SecretEnv
	data, _ := json.Marshal(map[string]string{identity.EnvClientID: "example-agent", identity.EnvClientSecret: string(secret)})
	if err = json.Unmarshal(data, &env); err != nil {
		return err
	}
	if _, err = fromEnv.AcquireForMCP(ctx, ref, env, "http://gateway:8080/mcp"); err != nil {
		return fmt.Errorf("agent env identity issuance failed")
	}
	bad := resolverConfig()
	bad.Bindings[0].ClientID = "unrelated-client"
	wrong, err := identity.New(bad, fixtureServer)
	if err != nil {
		return err
	}
	if _, err = wrong.AcquireForMCP(ctx, ref, nil, "http://gateway:8080/mcp"); err == nil {
		return fmt.Errorf("wrong Keycloak client accepted")
	}
	fmt.Println("PASS static/external/agent-env sources verified real Keycloak identity; wrong client and unapproved MCP denied")
	return nil
}
