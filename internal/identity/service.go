package identity

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/baseurl"
)

// Service has no background renewal loop and never accepts selectors from a workload.
type Service struct {
	server   string
	agentEnv bool
	// resolver is nil when only the agent env supplies credentials.
	resolver Resolver
	issuer   *issuer
	mcp      map[string]bool
	anon     map[string]bool
	debug    Debug
}

func New(c Config, server string) (*Service, error) {
	parsed, _ := url.Parse(server)
	sources := c.AgentEnv || len(c.Bindings) > 0 || c.External != nil
	if c.Version != 1 || !endpoint(server, c.AllowHTTP) || !baseurl.Path(parsed) || strings.HasSuffix(server, "/") || len(c.Bindings) > 0 && c.External != nil || !sources {
		return nil, ErrDenied
	}
	issued, err := newIssuer(c.Issuer, c.AllowHTTP)
	if err != nil {
		return nil, err
	}
	s := &Service{server: server, agentEnv: c.AgentEnv, issuer: issued}
	switch {
	case c.External != nil:
		if !endpoint(c.External.URL, c.AllowHTTP) || !filepath.IsAbs(c.External.BearerFile) {
			return nil, ErrDenied
		}
		s.resolver = &remoteResolver{*c.External, server, httpClient(c.External.URL)}
	case len(c.Bindings) > 0:
		if s.resolver, err = s.static(c.Bindings); err != nil {
			return nil, err
		}
	}
	if err := s.configureMCP(c.MCP, c.MCPAnonymous, c.AllowHTTP); err != nil {
		return nil, err
	}
	return s, nil
}

// static enforces one client per agent, also across workspaces, and no shared client.
func (s *Service) static(bindings []Binding) (Resolver, error) {
	r := &staticResolver{bindings: map[Ref]Binding{}}
	agents, clients := map[string]string{}, map[string]string{}
	for _, b := range bindings {
		ref := Ref{s.server, b.WorkspaceID, b.AgentID}
		if !validRef(ref, s.server) || !text(b.ClientID, 256) || !filepath.IsAbs(b.SecretFile) || !validTokenRequest(b.Token) {
			return nil, ErrDenied
		}
		if _, exists := r.bindings[ref]; exists {
			return nil, ErrDenied
		}
		if (agents[b.AgentID] != "" && agents[b.AgentID] != b.ClientID) || (clients[b.ClientID] != "" && clients[b.ClientID] != b.AgentID) {
			return nil, ErrDenied
		}
		agents[b.AgentID], clients[b.ClientID] = b.ClientID, b.AgentID
		b.Token = copyTokenRequest(b.Token)
		r.bindings[ref] = b
	}
	return r, nil
}

// Resolve returns the agent's credentials: with agent_env, from env (the claim's agent
// custom_env) when it names them; otherwise from the configured resolver. One name
// alone denies; an agent without either falls back to the resolver.
func (s *Service) Resolve(ctx context.Context, r Ref, env Env) (Credentials, error) {
	if !validRef(r, s.server) || ctx.Err() != nil {
		return Credentials{}, ErrDenied
	}
	if s.agentEnv && env != nil {
		values := env.Allowed([]string{EnvClientID, EnvClientSecret})
		if len(values) > 0 {
			id, secret := values[EnvClientID], values[EnvClientSecret]
			if !text(id, 256) || !text(secret, 8192) {
				s.debug.printf("identity denied agent=%s: agent env needs both %s and %s without line breaks", r.AgentID, EnvClientID, EnvClientSecret)
				return Credentials{}, ErrDenied
			}
			s.debug.printf("identity source=agent_env agent=%s workspace=%s client=%s", r.AgentID, r.WorkspaceID, id)
			return Credentials{ClientID: id, secret: secret}, nil
		}
	}
	if s.resolver == nil {
		s.debug.printf("identity denied agent=%s: no %s in agent env and no bindings or resolver", r.AgentID, EnvClientID)
		return Credentials{}, ErrDenied
	}
	credentials, err := s.resolver.Resolve(ctx, r)
	if err != nil || !text(credentials.ClientID, 256) || !validTokenRequest(credentials.request) {
		s.debug.printf("identity denied agent=%s workspace=%s: no binding or resolver refused", r.AgentID, r.WorkspaceID)
		return Credentials{}, ErrDenied
	}
	s.debug.printf("identity source=configuration agent=%s workspace=%s client=%s", r.AgentID, r.WorkspaceID, credentials.ClientID)
	return credentials, nil
}

// Acquire re-resolves credentials on every issuance, allowing key rotation.
func (s *Service) Acquire(ctx context.Context, r Ref, env Env) (AccessToken, error) {
	c, err := s.Resolve(ctx, r, env)
	if err != nil {
		return AccessToken{}, ErrDenied
	}
	return s.issuer.issue(ctx, c)
}
