package identity

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/baseurl"
)

// HTTPService is an operator-approved credential recipient with a closed route set.
// It does not add tool permissions: the recipient authorizes the logical agent.
type HTTPService struct {
	Name   string      `json:"name"`
	Origin string      `json:"origin"`
	Routes []HTTPRoute `json:"routes"`
	// BearerFile explicitly selects a service credential instead of the agent JWT.
	// It requires an agent allowlist and is never a JWT-verification fallback.
	BearerFile     string              `json:"bearer_file,omitempty"`
	PathCredential *HTTPPathCredential `json:"path_credential,omitempty"`
	AgentIDs       []string            `json:"agent_ids,omitempty"`
	Headers        map[string]string   `json:"headers,omitempty"`
}

// HTTPRoute selects one exact method and either a path or a slash-ended prefix.
type HTTPRoute struct {
	Method     string `json:"method"`
	Path       string `json:"path,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"`
}

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func (s *Service) configureHTTP(services []HTTPService, allowHTTP bool) error {
	s.http = map[string]HTTPService{}
	if len(services) > 16 {
		return ErrDenied
	}
	for _, service := range services {
		u, err := url.Parse(service.Origin)
		if !serviceName.MatchString(service.Name) || s.http[service.Name].Name != "" || !endpoint(service.Origin, allowHTTP) || err != nil || !baseurl.Path(u) || u.RawPath != "" || len(service.Routes) == 0 || len(service.Routes) > 32 {
			return ErrDenied
		}
		if service.BearerFile != "" && (!filepath.IsAbs(service.BearerFile) || len(service.AgentIDs) == 0) {
			return ErrDenied
		}
		if !service.validPathCredential() {
			return ErrDenied
		}
		for _, id := range service.AgentIDs {
			if !uuid.MatchString(id) {
				return ErrDenied
			}
		}
		for name, value := range service.Headers {
			if !strings.HasPrefix(name, "X-") || !text(name, 128) || !text(value, 1024) || strings.ContainsAny(name, " :\t") {
				return ErrDenied
			}
		}
		for _, route := range service.Routes {
			if !slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}, route.Method) || (route.Path == "") == (route.PathPrefix == "") {
				return ErrDenied
			}
			selected := route.Path
			if route.PathPrefix != "" {
				selected = route.PathPrefix
				if !strings.HasSuffix(selected, "/") {
					return ErrDenied
				}
			}
			clean := strings.TrimSuffix(selected, "/")
			if !strings.HasPrefix(selected, "/") || clean == "" || path.Clean(clean) != clean || strings.ContainsAny(selected, "%?\\#\r\n\x00") || strings.Contains(selected, "//") {
				return ErrDenied
			}
		}
		s.http[service.Name] = cloneHTTPService(service)
	}
	return nil
}

// HTTPServices returns copies of approved recipients; workloads cannot add them.
func (s *Service) HTTPServices() []HTTPService {
	result := make([]HTTPService, 0, len(s.http))
	for _, service := range s.http {
		result = append(result, cloneHTTPService(service))
	}
	slices.SortFunc(result, func(a, b HTTPService) int { return strings.Compare(a.Name, b.Name) })
	return result
}

// HTTPServicesFor filters by agent and configured credential availability.
// ENV-only agents without IAM names do not receive JWT-backed HTTP helpers;
// partial pairs remain selected so normal credential validation fails explicitly.
func (s *Service) HTTPServicesFor(r Ref, env Env) []HTTPService {
	if !validRef(r, s.server) {
		return nil
	}
	configured := s.resolver != nil
	if s.agentEnv && env != nil && len(env.Allowed([]string{EnvClientID, EnvClientSecret})) > 0 {
		configured = true
	}
	result := []HTTPService{}
	for _, service := range s.HTTPServices() {
		if service.BearerFile == "" && service.PathCredential == nil && !configured {
			continue
		}
		if len(service.AgentIDs) == 0 || slices.Contains(service.AgentIDs, r.AgentID) {
			result = append(result, service)
		}
	}
	return result
}

// AcquireForHTTP checks the named configured recipient before contacting the issuer.
func (s *Service) AcquireForHTTP(ctx context.Context, r Ref, env Env, name string) (AccessToken, error) {
	service := s.http[name]
	if !validRef(r, s.server) || ctx.Err() != nil || service.Name == "" || len(service.AgentIDs) > 0 && !slices.Contains(service.AgentIDs, r.AgentID) {
		return AccessToken{}, ErrDenied
	}
	file := service.BearerFile
	if service.PathCredential != nil {
		file = service.PathCredential.File
	}
	if file != "" {
		bearer, err := readSecret(file)
		if err != nil || service.PathCredential != nil && !validPathSecret(bearer) {
			return AccessToken{}, ErrDenied
		}
		return NewAccessToken(bearer, time.Now().Add(30*time.Second)), nil
	}
	return s.Acquire(ctx, r, env)
}

// Header returns trusted noncredential metadata; the relay replaces caller values.
func (s HTTPService) Header() http.Header {
	header := http.Header{}
	for name, value := range s.Headers {
		header.Set(name, value)
	}
	return header
}

// Allows checks a clean relay path against this service's explicit method routes.
func (s HTTPService) Allows(method, requestPath string) bool {
	for _, route := range s.Routes {
		if method == route.Method && (route.Path != "" && requestPath == route.Path || route.PathPrefix != "" && strings.HasPrefix(requestPath, route.PathPrefix)) {
			return true
		}
	}
	return false
}
