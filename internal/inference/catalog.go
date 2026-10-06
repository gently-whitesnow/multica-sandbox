package inference

import (
	"context"
	"path/filepath"
)

func (s *Service) configureCatalogs() error {
	c := s.config
	if len(c.Catalogs) > 0 && c.CatalogExternal != nil {
		return ErrDenied
	}
	for _, b := range c.Catalogs {
		scope := Scope{Server: s.server, WorkspaceID: b.WorkspaceID}
		if _, exists := s.catalogs[scope]; exists || !s.validScope(scope) || !validCatalog(b.Catalog) {
			return ErrDenied
		}
		s.catalogs[scope] = cloneCatalog(b.Catalog)
	}
	if c.CatalogExternal != nil {
		external := *c.CatalogExternal
		if !endpoint(external.URL, c.AllowHTTP) || !filepath.IsAbs(external.BearerFile) {
			return ErrDenied
		}
		s.config.CatalogExternal = &external
	}
	return nil
}
func validCatalog(c Catalog) bool {
	if len(c.Models) == 0 || len(c.Models) > 64 {
		return false
	}
	if c.DefaultModel != "" {
		if _, ok := c.Models[c.DefaultModel]; !ok {
			return false
		}
	}
	for name, m := range c.Models {
		if !validModel(name) || m.Context < 1 || m.Context > 10000000 || m.Output < 1 || m.Output > m.Context {
			return false
		}
		if m.Thinking == nil {
			continue
		}
		seen := map[string]bool{}
		for _, level := range m.Thinking.SupportedLevels {
			if !ValidEffort(level.Value) || seen[level.Value] {
				return false
			}
			seen[level.Value] = true
		}
		if len(seen) == 0 || m.Thinking.DefaultLevel != "" && !seen[m.Thinking.DefaultLevel] {
			return false
		}
	}
	return true
}

// ValidEffort checks native variant token syntax, not model authorization.
func ValidEffort(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func cloneCatalog(c Catalog) Catalog {
	models := map[string]Model{}
	for name, m := range c.Models {
		if m.Thinking != nil {
			thinking := *m.Thinking
			thinking.SupportedLevels = append([]ThinkingLevel(nil), thinking.SupportedLevels...)
			m.Thinking = &thinking
		}
		models[name] = m
	}
	c.Models = models
	return c
}

func (s *Service) validScope(scope Scope) bool {
	return scope.Server == s.server && uuid.MatchString(scope.WorkspaceID)
}

// Catalog describes a runtime's capabilities for discovery; it never issues
// identity or grants access. Per-agent access stays with the gateway.
func (s *Service) Catalog(ctx context.Context, scope Scope) (Catalog, error) {
	if !s.validScope(scope) || ctx.Err() != nil {
		return Catalog{}, ErrDenied
	}
	if s.config.CatalogExternal != nil {
		var out struct {
			Version   int     `json:"version"`
			Workspace Scope   `json:"workspace"`
			Catalog   Catalog `json:"catalog"`
		}
		request := struct {
			Version   int   `json:"version"`
			Workspace Scope `json:"workspace"`
		}{1, scope}
		if s.remoteJSON(ctx, request, s.config.CatalogExternal, &out) != nil || out.Version != 1 || out.Workspace != scope || !validCatalog(out.Catalog) {
			return Catalog{}, ErrDenied
		}
		return cloneCatalog(out.Catalog), nil
	}
	c, ok := s.catalogs[scope]
	if !ok {
		return Catalog{}, ErrDenied
	}
	return cloneCatalog(c), nil
}
