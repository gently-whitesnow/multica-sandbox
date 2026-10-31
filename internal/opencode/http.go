package opencode

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

// httpIdentity is optional; managed HTTP clients reuse the identity source and
// lifecycle without adding connections to the agent's selected MCP tool catalogue.
type httpIdentity interface {
	HTTPServicesFor(identity.Ref) []identity.HTTPService
	AcquireForHTTP(context.Context, identity.Ref, identity.Env, string) (identity.AccessToken, error)
}

func (r *running) configureHTTP() error {
	r.httpServices = map[string]identity.HTTPService{}
	r.httpTokens = map[string]identity.AccessToken{}
	source, ok := r.adapter.Issuer.(httpIdentity)
	if !ok {
		return nil
	}
	ref := identity.Ref{Server: r.adapter.Server, WorkspaceID: r.task.WorkspaceID, AgentID: r.task.AgentID}
	for _, service := range source.HTTPServicesFor(ref) {
		r.httpServices[service.Name] = service
	}
	if len(r.httpServices) > 0 && (r.adapter.MCPRelay == nil || r.adapter.MCPRelayURL == "") {
		return fmt.Errorf("HTTP identity services require mcp_relay: %w", ErrDenied)
	}
	return nil
}

func (r *running) refreshHTTP(ctx context.Context, ref identity.Ref) error {
	for name := range r.httpServices {
		r.mu.Lock()
		token := r.httpTokens[name]
		r.mu.Unlock()
		if time.Until(token.ExpiresAt) <= 10*time.Second {
			var err error
			token, err = r.adapter.Issuer.(httpIdentity).AcquireForHTTP(ctx, ref, r.task.Agent.CustomEnv, name)
			if err != nil || time.Until(token.ExpiresAt) <= 10*time.Second {
				return fmt.Errorf("HTTP identity issuance: %w", ErrDenied)
			}
		}
		r.mu.Lock()
		r.httpTokens[name] = token
		r.mu.Unlock()
	}
	return ctx.Err()
}

// httpEnvironment exposes only the attempt-bound opaque relay credential, never JWTs.
func (r *running) httpEnvironment(env map[string]string) {
	for name := range r.httpServices {
		prefix := "SANDBOX_HTTP_" + strings.ToUpper(name)
		env[prefix+"_URL"] = strings.TrimRight(r.adapter.MCPRelayURL, "/") + "/http/" + name
		env[prefix+"_TOKEN"] = r.mcpToken
	}
}

func (r *running) httpUpstream(method, requestPath string) (*url.URL, string, string, bool) {
	parts := strings.SplitN(strings.TrimPrefix(requestPath, "/http/"), "/", 2)
	if len(parts) != 2 {
		return nil, "", "", false
	}
	service, ok := r.httpServices[parts[0]]
	path := "/" + parts[1]
	if !ok || !service.Allows(method, path) {
		return nil, "", "", false
	}
	r.mu.Lock()
	token := r.httpTokens[service.Name]
	r.mu.Unlock()
	origin, err := url.Parse(service.Origin)
	if err != nil || time.Until(token.ExpiresAt) <= time.Second {
		return nil, "", "", false
	}
	return origin, path, token.Bearer(), true
}
