package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"strings"
	"time"
)

type policy struct {
	config   configuration
	provider *oidc.Provider
}

func (p *policy) verify(audience, role string) auth.TokenVerifier {
	verifier := p.provider.Verifier(&oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{"RS256"}})
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		parsed, err := verifier.Verify(ctx, token)
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		var claims struct {
			Client string `json:"azp"`
			Type   string `json:"typ"`
			Realm  struct {
				Roles []string `json:"roles"`
			} `json:"realm_access"`
		}
		if parsed.Claims(&claims) != nil || claims.Client != "agent-demo" || claims.Type != "Bearer" || parsed.Subject != agentID {
			return nil, auth.ErrInvalidToken
		}
		allowed := false
		for _, value := range claims.Realm.Roles {
			if value == role {
				allowed = true
			}
		}
		if !allowed {
			return nil, auth.ErrInvalidToken
		}
		var state struct {
			Status string `json:"status"`
		}
		status, err := exchange(ctx, "GET", "http://multica:8080/api/daemon/tasks/"+taskID+"/status", session(p.config), nil, &state)
		if err != nil || status != 200 || state.Status != "running" {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: parsed.Subject, Expiration: parsed.Expiry}, nil
	}
}
func serveMCP() error {
	c, err := load()
	if err != nil {
		return err
	}
	ctx := oidc.ClientContext(context.Background(), client)
	var provider *oidc.Provider
	for n := 0; n < 90; n++ {
		provider, err = oidc.NewProvider(ctx, issuer)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("issuer discovery unavailable")
	}
	p := &policy{c, provider}
	server := mcp.NewServer(&mcp.Implementation{Name: "external-fixture-tools", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "read_task", Description: "Read this identity's assigned Multica issue"}, p.readTask)
	mcp.AddTool(server, &mcp.Tool{Name: "read_reference", Description: "Read the reference word authorized for this identity"}, p.readReference)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", auth.RequireBearerToken(p.verify("sandbox-mcp", "fixture-reader"), nil)(handler))
	mux.HandleFunc("/authorize-inference", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		_, err := p.verify("sandbox-inference", "inference-demo")(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), r)
		if err != nil {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"agent": agentID, "workspace": workspace, "model": "demo"})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	httpServer := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, MaxHeaderBytes: 16 << 10}
	return httpServer.ListenAndServe()
}

type readInput struct {
	Issue string `json:"issue"`
}

func (p *policy) readTask(ctx context.Context, _ *mcp.CallToolRequest, args readInput) (*mcp.CallToolResult, map[string]any, error) {
	if args.Issue != issueID {
		return nil, nil, fmt.Errorf("denied")
	}
	var issue map[string]any
	status, err := exchange(ctx, "GET", "http://multica:8080/api/issues/"+issueID, session(p.config), nil, &issue)
	if err != nil || status != 200 {
		return nil, nil, fmt.Errorf("task unavailable")
	}
	fmt.Println("AUDIT allowed read_task: real Multica issue read")
	return nil, map[string]any{"title": issue["title"], "description": issue["description"]}, nil
}
func (p *policy) readReference(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
	fmt.Println("AUDIT allowed read_reference: fixture principal authorized")
	return nil, map[string]string{"word": p.config.Reference}, nil
}
