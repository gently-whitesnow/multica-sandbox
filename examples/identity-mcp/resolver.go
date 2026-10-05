package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

const serviceSubject = "30000000-0000-4000-8000-000000000001"

func resolverConfig() identity.Config {
	return identity.Config{Version: 1, Server: "https://multica.example.invalid", AllowHTTP: true, Issuers: []identity.IssuerConfig{{Name: "fixture", URL: issuer, TokenURL: issuer + "/protocol/openid-connect/token", JWKSURL: issuer + "/protocol/openid-connect/certs", MaxTTLSeconds: 180}}, MCP: []identity.MCPRule{{URL: "http://gateway:8080/mcp", Issuer: "fixture"}}, Bindings: []identity.Binding{{WorkspaceID: "10000000-0000-4000-8000-000000000001", AgentID: "20000000-0000-4000-8000-000000000001", Principal: identity.Principal{Issuer: "fixture", ClientID: "example-agent", Subject: serviceSubject}, SecretFile: "/secrets/client"}}}
}

// Both resolution sources must obtain and verify the same real Keycloak principal.
func checkResolvers(ctx context.Context) error {
	c := resolverConfig()
	ref := identity.Ref{Server: c.Server, WorkspaceID: c.Bindings[0].WorkspaceID, AgentID: c.Bindings[0].AgentID}
	static, err := identity.New(c)
	if err != nil {
		return err
	}
	if _, err = static.AcquireForMCP(ctx, ref, "http://gateway:8080/mcp"); err != nil {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "agent": ref, "issuer": "fixture", "client_id": "example-agent", "subject": serviceSubject, "client_secret": string(secret)})
	}))
	defer remote.Close()
	c.Bindings = nil
	c.External = &identity.ExternalConfig{URL: remote.URL, BearerFile: "/secrets/admin"}
	external, err := identity.New(c)
	if err != nil {
		return err
	}
	if _, err = external.AcquireForMCP(ctx, ref, "http://gateway:8080/mcp"); err != nil {
		return fmt.Errorf("external identity issuance failed")
	}
	if _, err = external.AcquireForMCP(ctx, ref, "http://unapproved:8080/mcp"); err != identity.ErrDenied {
		return fmt.Errorf("unapproved MCP delivery accepted")
	}
	bad := resolverConfig()
	bad.Bindings[0].Subject = "unrelated-principal"
	wrong, err := identity.New(bad)
	if err != nil {
		return err
	}
	if _, err = wrong.AcquireForMCP(ctx, ref, "http://gateway:8080/mcp"); err == nil {
		return fmt.Errorf("wrong Keycloak subject accepted")
	}
	fmt.Println("PASS static/external resolvers verified real Keycloak identity; wrong subject and unapproved MCP denied")
	return nil
}
