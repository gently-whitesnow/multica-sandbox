package identity

import (
	"context"
	"net/url"
)

// AcquireForMCP obtains identity only after the adapter recognizes an explicit
// IAM header in trusted Multica configuration. Network policy owns reachability.
func (s *Service) AcquireForMCP(ctx context.Context, r Ref, env Env, selectedURL string) (AccessToken, error) {
	u, err := url.Parse(selectedURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && (!s.allowHTTP || u.Scheme != "http")) {
		return AccessToken{}, ErrDenied
	}
	return s.Acquire(ctx, r, env)
}
