package identity

import "context"

func (s *Service) configureMCP(urls []string, allowHTTP bool) error {
	s.mcp = map[string]bool{}
	for _, u := range urls {
		if !endpoint(u, allowHTTP) || s.mcp[u] {
			return ErrDenied
		}
		s.mcp[u] = true
	}
	return nil
}

// AcquireForMCP is called only for a connection selected by trusted Multica data.
// It approves a delivery target; it neither connects to MCP nor checks tool roles.
func (s *Service) AcquireForMCP(ctx context.Context, r Ref, env Env, selectedURL string) (AccessToken, error) {
	if !s.mcp[selectedURL] {
		return AccessToken{}, ErrDenied
	}
	return s.Acquire(ctx, r, env)
}
