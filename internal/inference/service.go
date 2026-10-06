package inference

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type Service struct {
	config   Config
	server   string
	gateways map[string]bool
	bindings map[Scope]Binding
	catalogs map[Scope]Catalog
	client   *http.Client
}

func New(c Config, server string) (*Service, error) {
	parsed, _ := url.Parse(server)
	if c.Version != 1 || parsed == nil || parsed.Path != "" || !endpoint(server, c.AllowHTTP) || len(c.Gateways) == 0 || (len(c.Bindings) > 0) == (c.External != nil) {
		return nil, ErrDenied
	}
	if c.External != nil {
		external := *c.External
		c.External = &external
	}
	s := &Service{config: c, server: server, gateways: map[string]bool{}, bindings: map[Scope]Binding{}, catalogs: map[Scope]Catalog{}, client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, g := range c.Gateways {
		if !origin(g, c.AllowHTTP) || s.gateways[g] {
			return nil, ErrDenied
		}
		s.gateways[g] = true
	}
	for _, b := range c.Bindings {
		scope := Scope{Server: server, WorkspaceID: b.WorkspaceID}
		if _, exists := s.bindings[scope]; exists || !s.validScope(scope) || !s.gateways[b.Gateway] || !filepath.IsAbs(b.KeyFile) {
			return nil, ErrDenied
		}
		s.bindings[scope] = b
	}
	if c.External != nil && (!endpoint(c.External.URL, c.AllowHTTP) || !filepath.IsAbs(c.External.BearerFile)) {
		return nil, ErrDenied
	}
	if err := s.configureCatalogs(); err != nil {
		return nil, err
	}
	return s, nil
}

// Acquire resolves the workspace's gateway and key for one new attempt. Static key
// files are re-read each time, so a changed key applies to new attempts only.
func (s *Service) Acquire(ctx context.Context, scope Scope) (Target, error) {
	if !s.validScope(scope) || ctx.Err() != nil {
		return Target{}, ErrDenied
	}
	var target Target
	if s.config.External != nil {
		var err error
		if target, err = s.remote(ctx, scope); err != nil {
			return Target{}, ErrDenied
		}
	} else {
		binding, ok := s.bindings[scope]
		if !ok {
			return Target{}, ErrDenied
		}
		data, err := readFile(binding.KeyFile, 4096)
		if err != nil {
			return Target{}, ErrDenied
		}
		target = Target{Gateway: binding.Gateway, Key: Key{strings.TrimSpace(string(data))}}
	}
	if !s.gateways[target.Gateway] || !target.Key.valid() {
		return Target{}, ErrDenied
	}
	return target, nil
}
