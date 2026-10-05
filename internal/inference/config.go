// Package inference resolves trusted gateway/model bindings separately from MCP.
package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

var ErrDenied = errors.New("inference identity unavailable or denied")
var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type Config struct {
	Version      int                      `json:"version"`
	Server       string                   `json:"server"`
	IdentityFile string                   `json:"identity_file"`
	AllowHTTP    bool                     `json:"allow_http,omitempty"`
	Gateways     []Gateway                `json:"gateways"`
	Bindings     []Binding                `json:"bindings,omitempty"`
	External     *identity.ExternalConfig `json:"external,omitempty"`
}
type Gateway struct {
	URL    string `json:"url"`
	Issuer string `json:"issuer"`
}
type Model struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}
type Target struct {
	Gateway
	Model  string           `json:"model"`
	Models map[string]Model `json:"models"`
}
type Binding struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
	Target
}

func ReadConfig(path string) (Config, error) {
	var c Config
	b, err := readFile(path, 65536)
	if err != nil || decode(b, &c) != nil {
		return c, ErrDenied
	}
	return c, nil
}
func decode(b []byte, out any) error {
	if len(b) > 65536 {
		return ErrDenied
	}
	d := json.NewDecoder(bytes.NewReader(b))
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
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrDenied
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrDenied
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, ErrDenied
	}
	return b, nil
}
func endpoint(s string, allowHTTP bool) bool {
	u, err := url.Parse(s)
	return err == nil && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == "" && !strings.HasSuffix(s, "/") && (u.Scheme == "https" || allowHTTP && u.Scheme == "http")
}

func validModel(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
