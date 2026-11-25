package opencode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const mcpAttemptContextHeader = "X-Idp-Attempt-Context"

// MCPAttemptContextConfig is controller-only configuration for one MCP endpoint.
type MCPAttemptContextConfig struct {
	URL                 string `json:"url"`
	Audience            string `json:"audience"`
	KeyFile             string `json:"key_file"`
	RestrictIAMToTarget bool   `json:"restrict_iam_to_target,omitempty"`
}

type mcpAttemptContext struct {
	url, audience, server string
	key                   []byte
	restrictIAMToTarget   bool
}

// NewMCPRelayWithAttemptContext loads the opt-in signer before the relay starts.
func NewMCPRelayWithAttemptContext(c MCPAttemptContextConfig, server string) (*MCPRelay, error) {
	if !validMCPURL(c.URL) || strings.Contains(c.URL, "#") {
		return nil, fmt.Errorf("mcp_attempt_context.url must be an exact HTTP(S) MCP endpoint")
	}
	if !validMCPURL(server) || strings.ContainsAny(server, "?#") || len(server) > 256 || !visibleASCII(server) {
		return nil, fmt.Errorf("mcp_attempt_context requires a valid Multica server URL")
	}
	if c.Audience == "" || len(c.Audience) > 256 || !visibleASCII(c.Audience) {
		return nil, fmt.Errorf("mcp_attempt_context.audience must be a nonempty visible ASCII identifier of at most 256 characters")
	}
	key, err := readMCPAttemptKey(c.KeyFile)
	if err != nil {
		return nil, err
	}
	m := NewMCPRelay()
	m.attemptContext = &mcpAttemptContext{
		url: c.URL, audience: c.Audience, server: server, key: key, restrictIAMToTarget: c.RestrictIAMToTarget,
	}
	return m, nil
}

func readMCPAttemptKey(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("mcp_attempt_context.key_file must be an absolute path")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mcp_attempt_context.key_file must be a readable regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read mcp_attempt_context.key_file")
	}
	defer f.Close()
	const maxKeyFile = 4096
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil || len(raw) > maxKeyFile {
		return nil, fmt.Errorf("mcp_attempt_context.key_file could not be read within the 4096-byte limit")
	}
	key := strings.TrimSpace(string(raw))
	if len(key) < 32 || !visibleASCII(key) {
		return nil, fmt.Errorf("mcp_attempt_context.key_file must contain at least 32 visible ASCII characters")
	}
	return []byte(key), nil
}

func visibleASCII(s string) bool {
	for _, c := range s {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

type mcpAttemptPayload struct {
	Version     int    `json:"v"`
	Audience    string `json:"aud"`
	Source      string `json:"source"`
	Server      string `json:"server"`
	WorkspaceID string `json:"workspaceId"`
	AgentID     string `json:"agentId"`
	TaskID      string `json:"taskId"`
	Attempt     string `json:"attempt"`
	TokenHash   string `json:"tokenHash"`
	IssuedAt    int64  `json:"iat"`
}

func (s *mcpAttemptContext) add(lease *relay.Lease, r *running, connection Remote) {
	lease.Header.Del(mcpAttemptContextHeader)
	if s == nil || connection.URL != s.url || !connection.identity || lease.Credential == "" || !r.task.ValidAttempt() || len(lease.Attempt) > 256 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	confirmed := !r.ended && now.Sub(r.confirmed) <= attemptLease
	r.mu.Unlock()
	if !confirmed {
		return
	}
	payload, err := json.Marshal(mcpAttemptPayload{
		Version: 1, Audience: s.audience, Source: "multica-sandbox", Server: s.server,
		WorkspaceID: r.task.WorkspaceID, AgentID: r.task.AgentID, TaskID: r.task.ID,
		Attempt: lease.Attempt, TokenHash: attempt.Fingerprint(lease.Credential), IssuedAt: now.Unix(),
	})
	if err != nil {
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.key)
	_, _ = io.WriteString(mac, encoded)
	if lease.Header == nil {
		lease.Header = make(http.Header)
	}
	lease.Header.Set(mcpAttemptContextHeader, encoded+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}
