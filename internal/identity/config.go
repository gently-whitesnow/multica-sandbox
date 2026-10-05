package identity

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxDocument = 65536

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type Config struct {
	Version   int             `json:"version"`
	AllowHTTP bool            `json:"allow_http,omitempty"`
	Issuers   []IssuerConfig  `json:"issuers"`
	Bindings  []Binding       `json:"bindings,omitempty"`
	External  *ExternalConfig `json:"external,omitempty"`
	MCP       []MCPRule       `json:"mcp,omitempty"`
}
type IssuerConfig struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	TokenURL      string `json:"token_url"`
	JWKSURL       string `json:"jwks_url"`
	MaxTTLSeconds int    `json:"max_ttl_seconds,omitempty"`
}

// TokenRequest contains OAuth issuance parameters, not tool permissions.
type TokenRequest struct {
	Scopes   []string `json:"scopes,omitempty"`
	Resource string   `json:"resource,omitempty"`
}

// MCPRule approves delivery of an issuer's identity to one exact selected URL.
type MCPRule struct {
	URL    string `json:"url"`
	Issuer string `json:"issuer"`
}
type Binding struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
	Principal
	SecretFile string       `json:"secret_file"`
	Token      TokenRequest `json:"token,omitempty"`
}
type ExternalConfig struct {
	URL        string `json:"url"`
	BearerFile string `json:"bearer_file"`
}

func ReadConfig(path string) (Config, error) {
	var c Config
	data, err := readFile(path, maxDocument)
	if err != nil || strictJSON(data, &c) != nil {
		return c, ErrDenied
	}
	return c, nil
}
func strictJSON(data []byte, out any) error {
	if len(data) > maxDocument {
		return ErrDenied
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrDenied
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrDenied
	}
	return nil
}
func readFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrDenied
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrDenied
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrDenied
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, ErrDenied
	}
	return data, nil
}
func readSecret(path string) (string, error) {
	data, err := readFile(path, 8192)
	value := strings.TrimSpace(string(data))
	if err != nil || !text(value, 8192) {
		return "", ErrDenied
	}
	return value, nil
}
func text(s string, limit int) bool {
	return len(s) > 0 && len(s) <= limit && !strings.ContainsAny(s, "\r\n\x00")
}
func endpoint(s string, allowHTTP bool) bool {
	u, err := url.Parse(s)
	return err == nil && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && (u.Scheme == "https" || allowHTTP && u.Scheme == "http") && u.Opaque == ""
}
func validRef(r Ref, server string) bool {
	return r.Server == server && uuid.MatchString(r.WorkspaceID) && uuid.MatchString(r.AgentID)
}

func validTokenRequest(r TokenRequest) bool {
	if len(r.Scopes) > 64 || (r.Resource != "" && (!text(r.Resource, 1024) || !endpoint(r.Resource, true))) {
		return false
	}
	for _, scope := range r.Scopes {
		if !text(scope, 256) || strings.ContainsAny(scope, " \t") {
			return false
		}
	}
	return true
}
func copyTokenRequest(r TokenRequest) TokenRequest {
	r.Scopes = append([]string(nil), r.Scopes...)
	return r
}
