package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

type mcpConnection struct {
	URL         string            `json:"url"`
	Type        string            `json:"type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
	Disabled    bool              `json:"disabled,omitempty"`
	OAuth       json.RawMessage   `json:"oauth,omitempty"`
	Command     json.RawMessage   `json:"command,omitempty"`
	Args        json.RawMessage   `json:"args,omitempty"`
	Env         json.RawMessage   `json:"env,omitempty"`
	Environment json.RawMessage   `json:"environment,omitempty"`
	Timeout     json.RawMessage   `json:"timeout,omitempty"`
}

type Remote struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	headers   map[string]string
	headerEnv []string
}

// Remote carries credentials only inside the controller, never through formatting or JSON.
func (r Remote) String() string {
	return fmt.Sprintf("Remote{Type:%s URL:[redacted] headers:[redacted]}", r.Type)
}
func (r Remote) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}{Type: r.Type, URL: "[redacted]"})
}
func (r Remote) GoString() string           { return r.String() }
func (r Remote) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

func Select(t multica.Task) (map[string]Remote, error) {
	if !t.ValidAttempt() || len(t.Agent.MCPConfig) > 65536 {
		return nil, ErrDenied
	}
	if len(t.RemoteMCPConnections) != 0 {
		return nil, fmt.Errorf("%w: broker MCP/OAuth connections are not supported; use an HTTP URL and headers", ErrDenied)
	}
	raw := bytes.TrimSpace(t.Agent.MCPConfig)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return map[string]Remote{}, nil
	}
	servers, err := mcpServers(raw)
	if err != nil {
		return nil, err
	}
	out := map[string]Remote{}
	for name, data := range servers {
		if !namePattern.MatchString(name) {
			return nil, fmt.Errorf("%w: invalid MCP server name", ErrDenied)
		}
		var connection mcpConnection
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || decodeMCP(data, &connection) != nil {
			return nil, fmt.Errorf("%w: MCP server %q has an invalid configuration", ErrDenied, name)
		}
		if connection.Disabled || connection.Enabled != nil && !*connection.Enabled {
			continue
		}
		selected, err := selectMCP(name, connection)
		if err != nil {
			return nil, err
		}
		out[name] = selected
	}
	return out, nil
}

func mcpServers(raw []byte) (map[string]json.RawMessage, error) {
	var document struct {
		Legacy json.RawMessage `json:"mcpServers"`
		Native json.RawMessage `json:"mcp"`
	}
	if decodeMCP(raw, &document) != nil || len(document.Legacy) > 0 && len(document.Native) > 0 {
		return nil, fmt.Errorf("%w: use one MCP object: mcpServers or mcp", ErrDenied)
	}
	servers := map[string]json.RawMessage{}
	entries := document.Legacy
	if len(document.Native) > 0 {
		entries = document.Native
	}
	if len(entries) > 0 && (json.Unmarshal(entries, &servers) != nil || servers == nil) || len(servers) > 16 {
		return nil, fmt.Errorf("%w: invalid MCP server map or more than 16 servers", ErrDenied)
	}
	return servers, nil
}

func selectMCP(name string, c mcpConnection) (Remote, error) {
	unsupported := ""
	switch {
	case c.Type == "local" || c.Type == "stdio" || len(c.Command) > 0 || len(c.Args) > 0:
		unsupported = "stdio MCP is not supported; use a remote HTTP MCP server"
	case len(c.OAuth) > 0 && !bytes.Equal(bytes.TrimSpace(c.OAuth), []byte("false")):
		unsupported = "OAuth MCP is not supported; use an HTTP authorization header"
	case len(c.Env) > 0 || len(c.Environment) > 0:
		unsupported = "per-server ENV is not supported; reference agent ENV in HTTP headers with {env:NAME}"
	case len(c.Timeout) > 0:
		unsupported = "per-server MCP timeout is not supported"
	case c.Type != "" && c.Type != "http" && c.Type != "remote" && c.Type != "streamable-http":
		unsupported = "only streamable HTTP MCP is supported"
	}
	if unsupported != "" {
		return Remote{}, fmt.Errorf("%w: MCP server %q: %s", ErrDenied, name, unsupported)
	}
	decodedURL, _ := url.QueryUnescape(c.URL)
	if strings.Contains(c.URL, "{env:") || strings.Contains(decodedURL, "{env:") {
		return Remote{}, fmt.Errorf("%w: MCP server %q URL cannot reference ENV; use {env:NAME} in HTTP headers", ErrDenied, name)
	}
	if !validMCPURL(c.URL) {
		return Remote{}, fmt.Errorf("%w: MCP server %q requires an HTTP(S) URL without user information or a fragment", ErrDenied, name)
	}
	headers, err := mcpHeaders(c.Headers)
	if err != nil {
		return Remote{}, fmt.Errorf("%w: MCP server %q has invalid or reserved headers", ErrDenied, name)
	}
	return Remote{Type: "remote", URL: c.URL, headers: headers}, nil
}

func decodeMCP(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrDenied
	}
	return nil
}
