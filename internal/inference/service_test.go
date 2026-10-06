package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

const testGateway = "https://gateway.example.invalid"
const testKey = "sk-workspace-fixture-key"

func testScope() Scope {
	return Scope{Server: "https://multica.example.invalid", WorkspaceID: "10000000-0000-4000-8000-000000000001"}
}
func testConfig(t *testing.T) Config {
	t.Helper()
	key := filepath.Join(t.TempDir(), "workspace-key")
	if err := os.WriteFile(key, []byte(testKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{Version: 1, Gateways: []string{testGateway}, Bindings: []Binding{{WorkspaceID: testScope().WorkspaceID, Gateway: testGateway, KeyFile: key}}}
}
func TestStaticBindingRereadsKeyAndIsolatesWorkspaces(t *testing.T) {
	c := testConfig(t)
	s, err := New(c, testScope().Server)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.Acquire(context.Background(), testScope())
	if err != nil || target.Gateway != testGateway || target.Key.Reveal() != testKey {
		t.Fatal("binding not resolved", err)
	}
	// A changed key applies to the next attempt; earlier targets keep their key.
	if err = os.WriteFile(c.Bindings[0].KeyFile, []byte("sk-rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := s.Acquire(context.Background(), testScope())
	if err != nil || next.Key.Reveal() != "sk-rotated" || target.Key.Reveal() != testKey {
		t.Fatal("key change not applied to new attempts only", err)
	}
	for _, other := range []Scope{{Server: testScope().Server, WorkspaceID: "10000000-0000-4000-8000-000000000002"}, {Server: "https://other.example.invalid", WorkspaceID: testScope().WorkspaceID}, {Server: testScope().Server}} {
		if _, err = s.Acquire(context.Background(), other); err == nil {
			t.Fatal("cross-workspace or cross-server binding accepted", other)
		}
	}
	for _, bad := range []string{"", "sk with space", strings.Repeat("k", 4097)} {
		if err = os.WriteFile(c.Bindings[0].KeyFile, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Acquire(context.Background(), testScope()); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	if err = os.Remove(c.Bindings[0].KeyFile); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Acquire(context.Background(), testScope()); err == nil {
		t.Fatal("missing key file accepted")
	}
}
func TestConfigBoundaries(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"gateway path": func(c *Config) { c.Gateways[0] += "/v1"; c.Bindings[0].Gateway = c.Gateways[0] },
		"unapproved":   func(c *Config) { c.Bindings[0].Gateway = "https://other.example.invalid" },
		"http": func(c *Config) {
			c.Gateways[0] = "http://gateway.example.invalid"
			c.Bindings[0].Gateway = c.Gateways[0]
		},
		"relative key file": func(c *Config) { c.Bindings[0].KeyFile = "workspace-key" },
		"duplicate":         func(c *Config) { c.Bindings = append(c.Bindings, c.Bindings[0]) },
		"workspace":         func(c *Config) { c.Bindings[0].WorkspaceID = "workspace" },
		"both sources": func(c *Config) {
			c.External = &identity.ExternalConfig{URL: "https://resolver.example.invalid", BearerFile: "/bearer"}
		},
		"no gateways": func(c *Config) { c.Gateways = nil },
	} {
		c := testConfig(t)
		mutate(&c)
		if _, err := New(c, testScope().Server); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	var c Config
	if decode([]byte(`{"version":1,"gateways":[],"bindings":[{"workspace_id":"w","agent_id":"a","gateway":"g","key_file":"/k"}]}`), &c) == nil {
		t.Fatal("agent-scoped binding accepted")
	}
}
func TestKeyNeverFormats(t *testing.T) {
	target := Target{Gateway: testGateway, Key: Key{testKey}}
	data, _ := json.Marshal(target)
	for _, out := range []string{string(data), fmt.Sprint(target), fmt.Sprintf("%+v %#v %s", target, target, target.Key)} {
		if strings.Contains(out, testKey) {
			t.Fatal("key formatted: " + out)
		}
	}
}
func TestExternalBindingFailsClosed(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "resolver")
	if err := os.WriteFile(secret, []byte("fixture-admin"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "spoof", "gateway", "empty key", "unknown", "oversize", "outage", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Version   int   `json:"version"`
					Workspace Scope `json:"workspace"`
				}
				if r.Header.Get("Authorization") != "Bearer fixture-admin" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Workspace != testScope() {
					t.Error("resolver request not authenticated/bound")
					w.WriteHeader(403)
					return
				}
				scope, target := req.Workspace, map[string]string{"gateway": testGateway, "key": testKey}
				switch mode {
				case "outage":
					w.WriteHeader(503)
					return
				case "redirect":
					http.Redirect(w, r, "http://127.0.0.1:1", 307)
					return
				case "oversize":
					_, _ = w.Write(make([]byte, 65537))
					return
				case "spoof":
					scope.WorkspaceID = "10000000-0000-4000-8000-000000000002"
				case "gateway":
					target["gateway"] = "https://unapproved.example.invalid"
				case "empty key":
					target["key"] = ""
				case "unknown":
					target["agent_id"] = "20000000-0000-4000-8000-000000000001"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "workspace": scope, "target": target})
			}))
			defer server.Close()
			c := testConfig(t)
			c.Bindings = nil
			c.AllowHTTP = true
			c.External = &identity.ExternalConfig{URL: server.URL, BearerFile: secret}
			s, err := New(c, testScope().Server)
			if err != nil {
				t.Fatal(err)
			}
			target, err := s.Acquire(context.Background(), testScope())
			if mode == "valid" {
				if err != nil || target.Key.Reveal() != testKey || target.Gateway != testGateway {
					t.Fatal("valid resolution failed", err)
				}
			} else if err == nil {
				t.Fatal("unsafe response accepted")
			}
		})
	}
}
