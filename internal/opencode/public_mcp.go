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

// managedMCP is optional so an existing issuer keeps its closed legacy contract
// unless the deployment explicitly enables public MCP egress.
type managedMCP interface{ ManagedMCP(string) bool }

func (a *Adapter) mcpModes(connections map[string]Remote) (map[string]bool, map[string]bool, error) {
	anonymous, public := map[string]bool{}, map[string]bool{}
	for name, connection := range connections {
		if len(connection.headers) == 0 && a.Issuer != nil {
			managed, known := a.Issuer.(managedMCP)
			if known && managed.ManagedMCP(connection.URL) {
				continue
			}
			if !known && a.MCPPublicProxy == nil && !a.Issuer.AnonymousMCP(connection.URL) {
				continue
			}
			if a.MCPPublicProxy == nil && a.Issuer.AnonymousMCP(connection.URL) {
				anonymous[name] = true
				continue
			}
		}
		if a.MCPPublicProxy == nil {
			return nil, nil, fmt.Errorf("selected MCP requires mcp_public_proxy or managed identity: %w", ErrDenied)
		}
		anonymous[name], public[name] = true, true
	}
	return anonymous, public, nil
}
