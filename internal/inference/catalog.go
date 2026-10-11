package inference

import (
	"context"
	"path/filepath"
)

func (s *Service) configureCatalogs() error {
	c := s.config
	sources := 0
	for _, set := range []bool{len(c.Catalogs) > 0, c.CatalogExternal != nil, c.GatewayCatalog != nil} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return ErrDenied
	}
	if c.GatewayCatalog != nil {
		if err := s.configureGatewayCatalog(*c.GatewayCatalog); err != nil {
			return err
		}
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
func validCatalog(c Catalog) bool { return validCatalogSize(c, 64) }
func validCatalogSize(c Catalog, limit int) bool {
	if len(c.Models) == 0 || len(c.Models) > limit {
		return false
	}
	if c.DefaultModel != "" {
		if _, ok := c.Models[c.DefaultModel]; !ok {
			return false
		}
	}
	for name, m := range c.Models {
		if !validModel(name) || m.Context < 1 || m.Context > 10000000 || m.Output < 1 || m.Output > m.Context || (m.API != "" && providers[m.API] == "") || !validCost(m.Cost) {
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

func validCost(c *Cost) bool {
	return c == nil || c.Input >= 0 && c.Output >= 0 && c.CacheRead >= 0 && c.Input < 1e6 && c.Output < 1e6 && c.CacheRead < 1e6
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
		if m.Cost != nil {
			cost := *m.Cost
			m.Cost = &cost
		}
		models[name] = m
	}
	c.Models = models
	return c
}

func (s *Service) validScope(scope Scope) bool {
	return scope.Server == s.server && uuid.MatchString(scope.WorkspaceID)
}

// Catalog describes a runtime's capabilities for discovery; it never grants access.
// Workspace model access stays with the gateway key.
func (s *Service) Catalog(ctx context.Context, scope Scope) (Catalog, error) {
	if !s.validScope(scope) || ctx.Err() != nil {
		return Catalog{}, ErrDenied
	}
	if s.gateway != nil {
		return s.gateway.catalog(ctx, scope)
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
