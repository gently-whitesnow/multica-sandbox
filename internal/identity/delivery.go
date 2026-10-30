package identity

import "context"

// configureMCP approves each URL once, either for the agent's token or anonymous.
func (s *Service) configureMCP(urls, anonymous []string, allowHTTP bool) error {
	s.mcp, s.anon = map[string]bool{}, map[string]bool{}
	for _, list := range []struct {
		urls []string
		into map[string]bool
	}{{urls, s.mcp}, {anonymous, s.anon}} {
		for _, u := range list.urls {
			if !endpoint(u, allowHTTP) || s.mcp[u] || s.anon[u] {
				return ErrDenied
			}
			list.into[u] = true
		}
	}
	return nil
}

// AnonymousMCP reports whether a selected URL is approved without the agent's token.
func (s *Service) AnonymousMCP(selectedURL string) bool { return s.anon[selectedURL] }

// AcquireForMCP is called only for a connection selected by trusted Multica data.
// It approves a delivery target; it neither connects to MCP nor checks tool roles.
func (s *Service) AcquireForMCP(ctx context.Context, r Ref, env Env, selectedURL string) (AccessToken, error) {
	if !s.mcp[selectedURL] {
		s.debug.printf("identity denied agent=%s mcp=%s: not in the mcp list", r.AgentID, selectedURL)
		return AccessToken{}, ErrDenied
	}
	s.debug.printf("identity for mcp=%s agent=%s", selectedURL, r.AgentID)
	return s.Acquire(ctx, r, env)
}
