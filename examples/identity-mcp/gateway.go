package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const issuer = "http://keycloak:8080/realms/sandbox-example"

type grant struct {
	ID, Workspace, Agent, Task, Attempt, Resource, TokenHash string
	Active                                                   bool
}
type registry struct {
	sync.RWMutex
	grants map[string]grant
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (g *registry) verifier(v *oidc.IDTokenVerifier) auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		parsed, err := v.Verify(ctx, token)
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		var claims struct {
			Client string `json:"azp"`
			Type   string `json:"typ"`
		}
		if parsed.Claims(&claims) != nil || claims.Client != "example-agent" || claims.Type != "Bearer" {
			return nil, auth.ErrInvalidToken
		}
		g.RLock()
		defer g.RUnlock()
		run, ok := g.grants[fingerprint(token)]
		if !ok || !run.Active {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: parsed.Subject, Expiration: parsed.Expiry, Extra: map[string]any{"grant": run}}, nil
	}
}
func (g *registry) admin(secret []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+string(secret))) != 1 {
			http.Error(w, "denied", 403)
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var run grant
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&run) != nil || len(run.TokenHash) != 64 || run.ID == "" || run.Workspace == "" || run.Attempt == "" || run.Agent != "agent-1" || run.Task == "" || run.Resource == "" {
			http.Error(w, "invalid grant", 400)
			return
		}
		g.Lock()
		g.grants[run.TokenHash] = run
		g.Unlock()
		w.WriteHeader(204)
	})
}
func gateway() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: 5 * time.Second})
	var provider *oidc.Provider
	var err error
	for {
		provider, err = oidc.NewProvider(ctx, issuer)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("issuer discovery timed out")
		case <-time.After(time.Second):
		}
	}
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	grants := &registry{grants: map[string]grant{}}
	server := mcp.NewServer(&mcp.Implementation{Name: "identity-contract-fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "read_fixture", Description: "Read one granted fixture resource"}, grants.read)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", auth.RequireBearerToken(grants.verifier(provider.Verifier(&oidc.Config{ClientID: "sandbox-mcp", SupportedSigningAlgs: []string{"RS256"}})), nil)(handler))
	mux.Handle("/grants", grants.admin(secret))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	return serve(":8080", mux)
}

type readArgs struct {
	Workspace string `json:"workspace"`
	Resource  string `json:"resource"`
}

func (g *registry) read(_ context.Context, r *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, map[string]string, error) {
	if r.Extra == nil || r.Extra.TokenInfo == nil {
		return nil, nil, fmt.Errorf("denied")
	}
	run, ok := r.Extra.TokenInfo.Extra["grant"].(grant)
	g.RLock()
	defer g.RUnlock()
	current := g.grants[run.TokenHash]
	if !ok || !current.Active || current.ID != run.ID || current.Workspace != args.Workspace || current.Resource != args.Resource {
		return nil, nil, fmt.Errorf("denied")
	}
	return nil, map[string]string{"value": "fixture content", "attempt": run.Attempt}, nil
}
