package identity

import (
	"context"
	"net/url"
	"path/filepath"
)

// Service has no background renewal loop and never accepts selectors from a workload.
type Service struct {
	server   string
	resolver Resolver
	issuers  map[string]*issuer
	mcp      map[string]string
}

func New(c Config) (*Service, error) {
	parsed, _ := url.Parse(c.Server)
	if c.Version != 1 || !endpoint(c.Server, c.AllowHTTP) || parsed.Path != "" || (len(c.Bindings) > 0) == (c.External != nil) || len(c.Issuers) == 0 {
		return nil, ErrDenied
	}
	s := &Service{server: c.Server, issuers: map[string]*issuer{}}
	issuerURLs := map[string]bool{}
	for _, entry := range c.Issuers {
		if issuerURLs[entry.URL] {
			return nil, ErrDenied
		}
		issuerURLs[entry.URL] = true
		if _, exists := s.issuers[entry.Name]; exists {
			return nil, ErrDenied
		}
		issued, err := newIssuer(entry, c.AllowHTTP)
		if err != nil {
			return nil, err
		}
		s.issuers[entry.Name] = issued
	}
	if c.External != nil {
		if !endpoint(c.External.URL, c.AllowHTTP) || !filepath.IsAbs(c.External.BearerFile) {
			return nil, ErrDenied
		}
		s.resolver = &remoteResolver{*c.External, c.Server, httpClient(c.External.URL)}
	} else {
		resolver, err := s.static(c.Bindings)
		if err != nil {
			return nil, err
		}
		s.resolver = resolver
	}
	if err := s.configureMCP(c.MCP, c.AllowHTTP); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) static(bindings []Binding) (Resolver, error) {
	r := &staticResolver{bindings: map[Ref]Binding{}}
	principals, clients := map[string]string{}, map[string]string{}
	agents := map[string]Principal{}
	for _, b := range bindings {
		ref := Ref{s.server, b.WorkspaceID, b.AgentID}
		if !validRef(ref, s.server) || !s.validPrincipal(b.Principal) || !filepath.IsAbs(b.SecretFile) || !validTokenRequest(b.Token) {
			return nil, ErrDenied
		}
		if _, exists := r.bindings[ref]; exists {
			return nil, ErrDenied
		}
		if existing, ok := agents[b.AgentID]; ok && existing != b.Principal {
			return nil, ErrDenied
		}
		agents[b.AgentID] = b.Principal
		pk, ck := b.Issuer+"\x00"+b.Subject, b.Issuer+"\x00"+b.ClientID
		if (principals[pk] != "" && principals[pk] != b.AgentID) || (clients[ck] != "" && clients[ck] != b.AgentID) {
			return nil, ErrDenied
		}
		principals[pk], clients[ck] = b.AgentID, b.AgentID
		b.Token = copyTokenRequest(b.Token)
		r.bindings[ref] = b
	}
	return r, nil
}
func (s *Service) validPrincipal(p Principal) bool {
	return s.issuers[p.Issuer] != nil && text(p.ClientID, 256) && text(p.Subject, 256)
}
func (s *Service) Resolve(ctx context.Context, r Ref) (Credentials, error) {
	if !validRef(r, s.server) {
		return Credentials{}, ErrDenied
	}
	credentials, err := s.resolver.Resolve(ctx, r)
	if err != nil || !s.validPrincipal(credentials.Principal) || !validTokenRequest(credentials.request) {
		return Credentials{}, ErrDenied
	}
	return credentials, nil
}

// Acquire re-resolves credentials on every issuance, allowing external key rotation.
func (s *Service) Acquire(ctx context.Context, r Ref) (AccessToken, error) {
	c, err := s.Resolve(ctx, r)
	if err != nil {
		return AccessToken{}, ErrDenied
	}
	return s.issuers[c.Issuer].issue(ctx, c)
}
