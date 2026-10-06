package identity

import "context"

func (s *Service) configureMCP(rules []MCPRule, allowHTTP bool) error {
	s.mcp = map[string]string{}
	for _, rule := range rules {
		if !endpoint(rule.URL, allowHTTP) || s.issuers[rule.Issuer] == nil || s.mcp[rule.URL] != "" {
			return ErrDenied
		}
		s.mcp[rule.URL] = rule.Issuer
	}
	return nil
}

// AcquireForMCP is called only for a connection selected by trusted Multica data.
// It approves a delivery target; it neither connects to MCP nor checks tool roles.
func (s *Service) AcquireForMCP(ctx context.Context, r Ref, selectedURL string) (AccessToken, error) {
	expected, ok := s.mcp[selectedURL]
	if !ok {
		return AccessToken{}, ErrDenied
	}
	c, err := s.Resolve(ctx, r)
	if err != nil || c.Issuer != expected {
		return AccessToken{}, ErrDenied
	}
	return s.issuers[c.Issuer].issue(ctx, c)
}
