package identity

import (
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/baseurl"
)

// HTTPPathCredential inserts a file-backed secret after a trusted path prefix.
type HTTPPathCredential struct {
	File   string `json:"file"`
	Prefix string `json:"prefix"`
}

var pathSecret = regexp.MustCompile(`^[A-Za-z0-9_~:.-]+$`)

func validPathSecret(secret string) bool {
	return len(secret) <= 8192 && secret != "." && secret != ".." && pathSecret.MatchString(secret)
}

func (s HTTPService) validPathCredential() bool {
	p := s.PathCredential
	if p == nil {
		return true
	}
	if s.BearerFile != "" || len(s.AgentIDs) == 0 || !filepath.IsAbs(p.File) || p.Prefix == "" || len(p.Prefix) > 1024 || !baseurl.Path(&url.URL{Path: p.Prefix}) || strings.ContainsAny(p.Prefix, " ?#\r\n\t\x00") {
		return false
	}
	for _, route := range s.Routes {
		if route.PathPrefix != "" {
			return false
		}
	}
	return true
}

func cloneHTTPService(s HTTPService) HTTPService {
	s.Routes, s.AgentIDs, s.Headers = slices.Clone(s.Routes), slices.Clone(s.AgentIDs), maps.Clone(s.Headers)
	if s.PathCredential != nil {
		copy := *s.PathCredential
		s.PathCredential = &copy
	}
	return s
}

// CredentialPath never accepts a secret that could introduce another path segment.
func (s HTTPService) CredentialPath(requestPath, secret string) (string, bool) {
	if s.PathCredential == nil {
		return requestPath, true
	}
	if !validPathSecret(secret) {
		return "", false
	}
	return s.PathCredential.Prefix + secret + requestPath, true
}
