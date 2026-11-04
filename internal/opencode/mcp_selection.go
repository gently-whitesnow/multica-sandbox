package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
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
	identity  bool
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

// maxMCPServers bounds the connections an attempt receives.
const maxMCPServers = 16

func Select(t multica.Task) (map[string]Remote, error) {
	out, _, err := Selection(t)
	return out, err
}

// Selection also names the servers left out: unsupported transports and features, broker
// connections and servers beyond the limit, by name order. Malformed entries, credential
// templates in URLs and invalid headers or IAM selection still deny the task.
func Selection(t multica.Task) (map[string]Remote, []Skipped, error) {
	if !t.ValidAttempt() || len(t.Agent.MCPConfig) > 65536 {
		return nil, nil, ErrDenied
	}
	var skipped []Skipped
	for i, raw := range t.RemoteMCPConnections {
		var broker struct {
			Key string `json:"contribution_key"`
		}
		name := fmt.Sprintf("remote-connection-%d", i+1)
		if json.Unmarshal(raw, &broker) == nil && namePattern.MatchString(broker.Key) {
			name = broker.Key
		}
		skipped = append(skipped, Skipped{name, "broker MCP connections are not supported"})
	}
	raw := bytes.TrimSpace(t.Agent.MCPConfig)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return map[string]Remote{}, skipped, nil
	}
	servers, err := mcpServers(raw)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]Remote{}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if !namePattern.MatchString(name) {
			return nil, nil, fmt.Errorf("%w: invalid MCP server name", ErrDenied)
		}
		var connection mcpConnection
		if data := servers[name]; bytes.Equal(bytes.TrimSpace(data), []byte("null")) || decodeMCP(data, &connection) != nil {
			return nil, nil, fmt.Errorf("%w: MCP server %q has an invalid configuration", ErrDenied, name)
		}
		if connection.Disabled || connection.Enabled != nil && !*connection.Enabled {
			continue
		}
		if reason := unsupportedMCP(connection); reason != "" {
			skipped = append(skipped, Skipped{name, reason})
			continue
		}
		selected, err := selectMCP(name, connection)
		if err != nil {
			return nil, nil, err
		}
		if len(out) == maxMCPServers {
			skipped = append(skipped, Skipped{name, fmt.Sprintf("over the %d-server limit (the first %d by name are connected)", maxMCPServers, maxMCPServers)})
			continue
		}
		out[name] = selected
	}
	return out, skipped, nil
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
	if len(entries) > 0 && (json.Unmarshal(entries, &servers) != nil || servers == nil) {
		return nil, fmt.Errorf("%w: invalid MCP server map", ErrDenied)
	}
	return servers, nil
}

// unsupportedMCP names a transport or feature the relay cannot serve; such servers are
// skipped, not connected.
func unsupportedMCP(c mcpConnection) string {
	switch {
	case c.Type == "local" || c.Type == "stdio" || len(c.Command) > 0 || len(c.Args) > 0:
		return "stdio MCP is not supported; use a remote HTTP MCP server"
	case len(c.OAuth) > 0 && !bytes.Equal(bytes.TrimSpace(c.OAuth), []byte("false")):
		return "OAuth MCP is not supported; use an HTTP authorization header"
	case len(c.Env) > 0 || len(c.Environment) > 0:
		return "per-server ENV is not supported; reference agent ENV in HTTP headers with {env:NAME}"
	case len(c.Timeout) > 0:
		return "per-server MCP timeout is not supported"
	case c.Type != "" && c.Type != "http" && c.Type != "remote" && c.Type != "streamable-http":
		return "only streamable HTTP MCP is supported"
	}
	return ""
}

func selectMCP(name string, c mcpConnection) (Remote, error) {
	decodedURL, _ := url.QueryUnescape(c.URL)
	if strings.Contains(c.URL, "{env:") || strings.Contains(decodedURL, "{env:") || containsIAMTemplate(c.URL) || containsIAMTemplate(decodedURL) {
		return Remote{}, fmt.Errorf("%w: MCP server %q URL cannot contain credential templates; use HTTP headers", ErrDenied, name)
	}
	if !validMCPURL(c.URL) {
		return Remote{}, fmt.Errorf("%w: MCP server %q requires an HTTP(S) URL without user information or a fragment", ErrDenied, name)
	}
	headers, err := mcpHeaders(c.Headers)
	if err != nil {
		return Remote{}, fmt.Errorf("%w: MCP server %q has invalid or reserved headers", ErrDenied, name)
	}
	managed, err := selectIAMHeader(headers)
	if err != nil {
		return Remote{}, fmt.Errorf("%w: MCP server %q: %s", ErrDenied, name, err)
	}
	return Remote{Type: "remote", URL: c.URL, headers: headers, identity: managed}, nil
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
