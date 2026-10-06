// Package inference resolves trusted workspace gateway bindings and advisory catalogs separately from MCP.
package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

var ErrDenied = errors.New("inference binding unavailable or denied")
var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// Config binds workspaces to an approved gateway origin and a controller-only key.
type Config struct {
	Version         int                      `json:"version"`
	AllowHTTP       bool                     `json:"allow_http,omitempty"`
	Gateways        []string                 `json:"gateways"`
	Bindings        []Binding                `json:"bindings,omitempty"`
	External        *identity.ExternalConfig `json:"external,omitempty"`
	Catalogs        []CatalogBinding         `json:"catalogs,omitempty"`
	CatalogExternal *identity.ExternalConfig `json:"catalog_external,omitempty"`
}
type Binding struct {
	WorkspaceID string `json:"workspace_id"`
	Gateway     string `json:"gateway"`
	KeyFile     string `json:"key_file"`
}
type Model struct {
	Label    string    `json:"label,omitempty"`
	Context  int       `json:"context"`
	Output   int       `json:"output"`
	Thinking *Thinking `json:"thinking,omitempty"`
}
type Thinking struct {
	SupportedLevels []ThinkingLevel `json:"supported_levels"`
	DefaultLevel    string          `json:"default_level,omitempty"`
}
type ThinkingLevel struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
type Catalog struct {
	DefaultModel string           `json:"default_model,omitempty"`
	Models       map[string]Model `json:"models"`
}

// CatalogBinding is runtime-scoped: Multica discovers and caches models per runtime,
// and the controller registers one runtime per workspace.
type CatalogBinding struct {
	WorkspaceID string `json:"workspace_id"`
	Catalog
}

// Scope is the inference principal: a workspace of the controller's Multica server.
type Scope struct {
	Server      string `json:"server"`
	WorkspaceID string `json:"workspace_id"`
}

// Key is a workspace gateway key. It never formats, marshals or logs its value.
type Key struct{ value string }

func (k *Key) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &k.value) != nil {
		return ErrDenied
	}
	return nil
}
func (Key) MarshalJSON() ([]byte, error) { return []byte(`null`), nil }
func (Key) String() string               { return "[redacted]" }
func (Key) GoString() string             { return "[redacted]" }
func (Key) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, "[redacted]") }

// Reveal returns the key for the relay grant only.
func (k Key) Reveal() string { return k.value }

func (k Key) valid() bool {
	if len(k.value) == 0 || len(k.value) > 4096 {
		return false
	}
	for _, r := range k.value {
		if r <= 32 || r == 127 {
			return false
		}
	}
	return true
}

// Target is where a workspace's attempts are relayed, with the key that replaces their credential.
type Target struct {
	Gateway string `json:"gateway"`
	Key     Key    `json:"key"`
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

// origin admits a gateway origin; the relay forwards only the OpenAI-compatible /v1 path.
func origin(s string, allowHTTP bool) bool {
	u, err := url.Parse(s)
	return err == nil && endpoint(s, allowHTTP) && u.Path == ""
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
