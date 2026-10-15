package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/baseurl"
)

// Workspace env names of a workspace's gateway connection (ADR 0022). EnvBaseURL is the
// OpenAI-compatible base URL including its /v1 path, as clients configure it.
const (
	EnvBaseURL = "INFERENCE_BASE_URL"
	EnvAPIKey  = "INFERENCE_API_KEY"
)

// Env is a redacted view of Multica env values.
type Env interface {
	Allowed(names []string) map[string]string
}

// WorkspaceEnv reads a workspace's Multica env; ok is false when the server has none.
type WorkspaceEnv func(ctx context.Context, workspaceID string) (env Env, ok bool, err error)

type Service struct {
	config   Config
	server   string
	env      WorkspaceEnv
	bindings map[Scope]Binding
	catalogs map[Scope]Catalog
	gateway  *gatewayCatalog
	client   *http.Client
}

func New(c Config, server string) (*Service, error) {
	parsed, _ := url.Parse(server)
	sources := len(c.Bindings) > 0 || c.External != nil || c.WorkspaceEnv
	if c.Version != 1 || parsed == nil || !baseurl.Path(parsed) || !endpoint(server, c.AllowHTTP) || len(c.Bindings) > 0 && c.External != nil || !sources {
		return nil, ErrDenied
	}
	if c.External != nil {
		external := *c.External
		c.External = &external
	}
	s := &Service{config: c, server: server, bindings: map[Scope]Binding{}, catalogs: map[Scope]Catalog{}, client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, b := range c.Bindings {
		scope := Scope{Server: server, WorkspaceID: b.WorkspaceID}
		if _, exists := s.bindings[scope]; exists || !s.validScope(scope) || !origin(b.Gateway, c.AllowHTTP) || !filepath.IsAbs(b.KeyFile) {
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

// UseWorkspaceEnv supplies the Multica reader that workspace_env requires.
func (s *Service) UseWorkspaceEnv(read WorkspaceEnv) { s.env = read }

// Acquire resolves the workspace's gateway and key for one new attempt or catalog read.
// Workspace env and static key files are re-read each time, so a changed key applies to
// new attempts only.
func (s *Service) Acquire(ctx context.Context, scope Scope) (Target, error) {
	if !s.validScope(scope) || ctx.Err() != nil {
		return Target{}, ErrDenied
	}
	target, found, err := s.fromEnv(ctx, scope)
	if err != nil {
		return Target{}, err
	}
	switch {
	case found:
	case s.config.External != nil:
		if target, err = s.remote(ctx, scope); err != nil {
			return Target{}, ErrDenied
		}
	default:
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
	if !origin(target.Gateway, s.config.AllowHTTP) || !target.Key.valid() {
		return Target{}, ErrDenied
	}
	return target, nil
}

// fromEnv reads the connection from the workspace's Multica env. A workspace without
// either name, or a server without workspace env, falls back to the configuration; one
// name alone, an unreadable env or a base URL without /v1 deny.
func (s *Service) fromEnv(ctx context.Context, scope Scope) (Target, bool, error) {
	if !s.config.WorkspaceEnv {
		return Target{}, false, nil
	}
	if s.env == nil {
		return Target{}, false, ErrDenied
	}
	env, ok, err := s.env(ctx, scope.WorkspaceID)
	if err != nil {
		return Target{}, false, fmt.Errorf("read workspace env: %w", ErrDenied)
	}
	values := map[string]string{}
	if ok {
		values = env.Allowed([]string{EnvBaseURL, EnvAPIKey})
	}
	base, key := values[EnvBaseURL], values[EnvAPIKey]
	if len(values) == 0 {
		return Target{}, false, nil
	}
	gateway, versioned := strings.CutSuffix(base, "/v1")
	if base == "" || key == "" || !versioned {
		return Target{}, false, fmt.Errorf("workspace env needs %s ending in /v1 and %s: %w", EnvBaseURL, EnvAPIKey, ErrDenied)
	}
	return Target{Gateway: gateway, Key: Key{strings.TrimSpace(key)}}, true, nil
}
