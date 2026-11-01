package opencode

import (
	"fmt"
	"net/url"
)

// ParseMCPPublicProxy accepts only a trusted HTTP(S) proxy origin, never credentials,
// routing paths or query parameters supplied by a selected connection.
func ParseMCPPublicProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("mcp_public_proxy must be an HTTP(S) proxy origin without credentials, path, query or fragment")
	}
	return u, nil
}

// mcpModes uses explicit native MCP authorization. Routing belongs to deployment,
// so IAM connections use the same proxy as anonymous/static-header connections.
func (a *Adapter) mcpModes(connections map[string]Remote) (map[string]bool, map[string]bool, error) {
	anonymous, public := map[string]bool{}, map[string]bool{}
	for name, connection := range connections {
		if connection.identity && a.Issuer == nil {
			return nil, nil, fmt.Errorf("MCP server %q requests IAM but identity_file is not configured: %w", name, ErrDenied)
		}
		anonymous[name] = !connection.identity
		public[name] = a.MCPPublicProxy != nil
	}
	return anonymous, public, nil
}
