package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const Version = "1.18.34"
const AuthPath = "/workspace/data/opencode/mcp-auth.json"

var ErrDenied = errors.New("OpenCode task projection denied")
var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

type Connection struct {
	URL     string            `json:"url"`
	Type    string            `json:"type,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Remote struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type Token struct {
	AccessToken string `json:"accessToken"`
	ExpiresAt   int64  `json:"expiresAt"`
}

type Entry struct {
	ServerURL string `json:"serverUrl"`
	Tokens    Token  `json:"tokens"`
}

func Select(t multica.Task) (map[string]Remote, error) {
	if !t.ValidAttempt() || len(t.RemoteMCPConnections) != 0 || len(t.Agent.MCPConfig) > 65536 {
		return nil, ErrDenied
	}
	var document struct {
		Servers map[string]Connection `json:"mcpServers"`
	}
	dec := json.NewDecoder(bytes.NewReader(t.Agent.MCPConfig))
	dec.DisallowUnknownFields()
	if dec.Decode(&document) != nil || len(document.Servers) > 16 {
		return nil, ErrDenied
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, ErrDenied
	}
	out := map[string]Remote{}
	for name, c := range document.Servers {
		if !namePattern.MatchString(name) || c.URL == "" || (c.Type != "" && c.Type != "http" && c.Type != "sse") || len(c.Headers) != 0 {
			return nil, ErrDenied
		}
		// No user headers: API keys and authorization aliases are credentials too.
		out[name] = Remote{Type: "remote", URL: c.URL}
	}
	return out, nil
}

func Config(connections map[string]Remote) ([]byte, error) {
	permissions := map[string]string{"*": "deny"}
	for name := range connections {
		permissions[name+"_*"] = "allow"
	}
	return json.Marshal(map[string]any{"mcp": connections, "autoupdate": false, "share": "disabled", "permission": permissions})
}

func Prompt(t multica.Task) ([]byte, error) {
	if t.Agent == nil || len(t.Repos) > 32 {
		return nil, ErrDenied
	}
	sources := []string{}
	for _, repo := range t.Repos {
		u, err := url.Parse(repo.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(repo.URL, "\r\n\x00") {
			return nil, ErrDenied
		}
		encoded, err := json.Marshal(repo)
		if err != nil {
			return nil, ErrDenied
		}
		sources = append(sources, string(encoded))
	}
	sections := []string{t.Agent.Instructions, t.WorkspaceContext, "Assigned issue: " + t.IssueID, t.ProjectTitle, t.ProjectDescription, t.TriggerCommentContent, t.ChatMessage}
	if len(sources) > 0 {
		sections = append(sections, "Repository references (no local checkout; access through selected authorized MCP):\n"+strings.Join(sources, "\n"))
	}
	if t.PriorSessionID != "" {
		sections = append(sections, "This disposable attempt starts a fresh native session. Prior session resume is unavailable.")
	}
	prompt := strings.Join(sections, "\n\n")
	if len(prompt) > 65536 {
		return nil, ErrDenied
	}
	return []byte(prompt), nil
}
