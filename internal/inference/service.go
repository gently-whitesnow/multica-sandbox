package inference

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

type Issuer interface {
	AcquireForIssuer(context.Context, identity.Ref, string) (identity.AccessToken, error)
}
type Session struct {
	Target
	Token identity.AccessToken
}
type Service struct {
	config   Config
	server   string
	issuer   Issuer
	gateways map[string]string
	bindings map[identity.Ref]Target
	catalogs map[Scope]Catalog
	client   *http.Client
}

func New(c Config, server string, issuer Issuer) (*Service, error) {
	parsed, _ := url.Parse(server)
	if c.Version != 1 || parsed == nil || parsed.Path != "" || !endpoint(server, c.AllowHTTP) || len(c.Gateways) == 0 || issuer == nil || (len(c.Bindings) > 0) == (c.External != nil) {
		return nil, ErrDenied
	}
	if c.External != nil {
		external := *c.External
		c.External = &external
	}
	s := &Service{config: c, server: server, issuer: issuer, gateways: map[string]string{}, bindings: map[identity.Ref]Target{}, catalogs: map[Scope]Catalog{}, client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, g := range c.Gateways {
		if !endpoint(g.URL, c.AllowHTTP) || g.Issuer == "" || s.gateways[g.URL] != "" {
			return nil, ErrDenied
		}
		s.gateways[g.URL] = g.Issuer
	}
	for _, b := range c.Bindings {
		ref := identity.Ref{Server: server, WorkspaceID: b.WorkspaceID, AgentID: b.AgentID}
		if !s.validRef(ref) || !s.validTarget(b.Target) {
			return nil, ErrDenied
		}
		if _, exists := s.bindings[ref]; exists {
			return nil, ErrDenied
		}
		s.bindings[ref] = b.Target
	}
	if c.External != nil && (!endpoint(c.External.URL, c.AllowHTTP) || !filepath.IsAbs(c.External.BearerFile)) {
		return nil, ErrDenied
	}
	if err := s.configureCatalogs(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) validRef(r identity.Ref) bool {
	return r.Server == s.server && uuid.MatchString(r.WorkspaceID) && uuid.MatchString(r.AgentID)
}
func (s *Service) validTarget(t Target) bool {
	return t.Issuer != "" && s.gateways[t.URL] == t.Issuer
}
func (s *Service) Acquire(ctx context.Context, ref identity.Ref) (Session, error) {
	if !s.validRef(ref) || ctx.Err() != nil {
		return Session{}, ErrDenied
	}
	target, err := s.resolve(ctx, ref)
	if err != nil || !s.validTarget(target) {
		return Session{}, ErrDenied
	}
	token, err := s.issuer.AcquireForIssuer(ctx, ref, target.Issuer)
	if err != nil {
		return Session{}, ErrDenied
	}
	return Session{target, token}, nil
}
func (s *Service) resolve(ctx context.Context, ref identity.Ref) (Target, error) {
	if s.config.External != nil {
		return s.remote(ctx, ref)
	}
	target, ok := s.bindings[ref]
	if !ok {
		return Target{}, ErrDenied
	}
	return target, nil
}
