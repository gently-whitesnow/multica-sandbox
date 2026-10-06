package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

type recordingInference struct {
	*inference.Service
	sync.Mutex
	issued map[string][]identity.AccessToken
}

func (s *recordingInference) Acquire(ctx context.Context, ref identity.Ref) (inference.Session, error) {
	session, err := s.Service.Acquire(ctx, ref)

	if err == nil {
		s.Lock()
		s.issued[ref.WorkspaceID] = append(s.issued[ref.WorkspaceID], session.Token)
		s.Unlock()
	}
	return session, err
}
func inferenceIdentityConfig() identity.Config {
	c := resolverConfig()
	c.MCP = nil
	c.Bindings[0].ClientID = "example-inference"
	c.Bindings[0].Subject = inferenceSubject
	c.Bindings[0].SecretFile = "/secrets/inference"
	second := c.Bindings[0]
	second.WorkspaceID = "10000000-0000-4000-8000-000000000002"
	c.Bindings = append(c.Bindings, second)
	return c
}
func fixtureInferenceTarget() inference.Target {
	return inference.Target{Gateway: inference.Gateway{URL: "http://litellm:4000/v1", Issuer: "fixture"}}
}
func configureRotationInference(ctx context.Context, adapter *opencode.Adapter) (*recordingInference, func(), error) {
	issuer, err := identity.New(inferenceIdentityConfig(), adapter.Server)
	if err != nil {
		return nil, nil, err
	}
	c := inference.Config{Version: 1, AllowHTTP: true, Gateways: []inference.Gateway{fixtureInferenceTarget().Gateway}}
	for _, b := range inferenceIdentityConfig().Bindings {
		c.Bindings = append(c.Bindings, inference.Binding{WorkspaceID: b.WorkspaceID, AgentID: b.AgentID, Target: fixtureInferenceTarget()})
	}
	workspaces := map[string]bool{}
	for _, b := range c.Bindings {
		if !workspaces[b.WorkspaceID] {
			workspaces[b.WorkspaceID] = true
			c.Catalogs = append(c.Catalogs, inference.CatalogBinding{WorkspaceID: b.WorkspaceID, Catalog: fixtureInferenceCatalog()})
		}
	}
	static, err := inference.New(c, adapter.Server, issuer)
	if err != nil {
		return nil, nil, err
	}
	ref := identity.Ref{Server: fixtureServer, WorkspaceID: c.Bindings[0].WorkspaceID, AgentID: c.Bindings[0].AgentID}
	if _, err := static.Acquire(ctx, ref); err != nil {
		return nil, nil, fmt.Errorf("static inference issuance failed")
	}
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return nil, nil, err
	}
	resolver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Version int          `json:"version"`
			Agent   identity.Ref `json:"agent"`
		}
		if r.Header.Get("Authorization") != "Bearer "+string(secret) || json.NewDecoder(r.Body).Decode(&req) != nil || req.Version != 1 || req.Agent.Server != fixtureServer || req.Agent.AgentID != ref.AgentID || (req.Agent.WorkspaceID != ref.WorkspaceID && req.Agent.WorkspaceID != "10000000-0000-4000-8000-000000000002") {
			w.WriteHeader(403)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "agent": req.Agent, "target": fixtureInferenceTarget()})
	}))
	c.Bindings = nil
	c.External = &identity.ExternalConfig{URL: resolver.URL, BearerFile: "/secrets/admin"}
	service, err := inference.New(c, adapter.Server, issuer)
	if err != nil {
		resolver.Close()
		return nil, nil, err
	}
	wrong := ref
	wrong.WorkspaceID = "10000000-0000-4000-8000-000000000003"
	if _, err := service.Acquire(ctx, wrong); err == nil {
		resolver.Close()
		return nil, nil, fmt.Errorf("cross-workspace inference selector accepted")
	}
	recorded := &recordingInference{Service: service, issued: map[string][]identity.AccessToken{}}
	adapter.Inference = recorded
	return recorded, resolver.Close, nil
}
func checkInferenceEvidence(ctx context.Context, recorded *recordingInference, tasks []multica.Task, mcp *recordingIssuer) error {
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	status, body, err := doRequest(ctx, "GET", "http://gateway:8080/evidence", nil, string(secret))
	if err != nil || status != 200 {
		return fmt.Errorf("inference evidence unavailable")
	}
	var counts map[string][2]int
	if json.Unmarshal(body, &counts) != nil {
		return fmt.Errorf("invalid inference evidence")
	}
	for _, task := range tasks {
		effort := task.Agent.ThinkingLevel
		if effort == "" {
			effort = "medium"
		}
		if counts["selection:"+task.AttemptKey()+"|fixture|"+effort][0] < 25 {
			return fmt.Errorf("explicit/default model reasoning did not reach gateway")
		}
		evidence := counts["inference:"+task.AttemptKey()]
		recorded.Lock()
		tokens := append([]identity.AccessToken(nil), recorded.issued[task.WorkspaceID]...)
		recorded.Unlock()
		if evidence[0] < 25 || evidence[1] < 3 || len(tokens) < 3 || time.Now().Before(tokens[1].ExpiresAt) {
			return fmt.Errorf("inference did not cross real expiries: requests=%d versions=%d", evidence[0], evidence[1])
		}
		latest := tokens[len(tokens)-1]
		if !latest.ExpiresAt.After(time.Now()) {
			return fmt.Errorf("ended-token check only covered expiry")
		}
		if err := deniedInference(ctx, latest.Bearer()); err != nil {
			return err
		}
		if err := deniedInference(ctx, tokens[0].Bearer()); err != nil {
			return err
		}
		if err := call(ctx, latest.Bearer(), task.WorkspaceID, "document", false); err != nil {
			return fmt.Errorf("inference identity accepted by MCP")
		}
		mcp.Lock()
		token := mcp.issued[task.WorkspaceID][0]
		mcp.Unlock()
		if err := deniedInference(ctx, token.Bearer()); err != nil {
			return err
		}
	}
	if err := deniedInference(ctx, ""); err != nil {
		return err
	}
	fmt.Println("PASS real LiteLLM streaming requests used at least three inference JWTs in each concurrent task; distinct MCP recipient, external model grants, no provider-key delivery, expiry and ended-attempt denial")
	return nil
}
func deniedInference(ctx context.Context, bearer string) error {
	status, _, err := doRequest(ctx, "POST", "http://litellm:4000/v1/chat/completions", map[string]any{"model": "fixture", "messages": []any{map[string]string{"role": "user", "content": "test"}}}, bearer)
	if err != nil || (status != 401 && status != 403) {
		return fmt.Errorf("invalid inference identity was not denied: %d", status)
	}
	return nil
}

func checkInferencePolicy(ctx context.Context, recorded *recordingInference, task multica.Task) error {
	recorded.Lock()
	token := recorded.issued[task.WorkspaceID][0]
	recorded.Unlock()
	secret, err := os.ReadFile("/secrets/upstream")
	if err != nil {
		return err
	}
	for _, mode := range []string{"allowed", "model", "routing"} {
		body := map[string]any{"model": "fixture", "stream": true, "messages": []any{map[string]string{"role": "user", "content": "Fixture reply"}}}
		if mode == "model" {
			body["model"] = "ungranted"
		}
		if mode == "routing" {
			body["api_base"] = "http://unapproved:8080/v1"
			body["api_key"] = "caller-key"
		}
		status, response, err := doRequest(ctx, "POST", "http://litellm:4000/v1/chat/completions", body, token.Bearer())
		if err != nil || mode == "allowed" && status != 200 || mode != "allowed" && status != 401 && status != 403 {
			return fmt.Errorf("gateway model/routing policy failed: %s status=%d", mode, status)
		}
		if bytes.Contains(response, secret) || bytes.Contains(response, []byte(token.Bearer())) {
			return fmt.Errorf("gateway response disclosed credentials")
		}
	}
	fmt.Println("PASS LiteLLM enforces external model grants and denies caller routing/keys; responses expose no fixture credentials")
	return nil
}

func fixtureInferenceCatalog() inference.Catalog {
	return inference.Catalog{DefaultModel: "fixture", Models: map[string]inference.Model{"fixture": {Label: "Fixture", Context: 64000, Output: 4096, Thinking: &inference.Thinking{DefaultLevel: "medium", SupportedLevels: []inference.ThinkingLevel{{Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}}}}}}
}
