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
	return Config{Version: 1, Bindings: []Binding{{WorkspaceID: testScope().WorkspaceID, Gateway: testGateway, KeyFile: key}}}
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
		"gateway slash":     func(c *Config) { c.Bindings[0].Gateway += "/inference/" },
		"gateway dots":      func(c *Config) { c.Bindings[0].Gateway += "/a/../b" },
		"gateway query":     func(c *Config) { c.Bindings[0].Gateway += "?x=1" },
		"gateway userinfo":  func(c *Config) { c.Bindings[0].Gateway = "https://user@gateway.example.invalid" },
		"http":              func(c *Config) { c.Bindings[0].Gateway = "http://gateway.example.invalid" },
		"relative key file": func(c *Config) { c.Bindings[0].KeyFile = "workspace-key" },
		"duplicate":         func(c *Config) { c.Bindings = append(c.Bindings, c.Bindings[0]) },
		"workspace":         func(c *Config) { c.Bindings[0].WorkspaceID = "workspace" },
		"both sources": func(c *Config) {
			c.External = &identity.ExternalConfig{URL: "https://resolver.example.invalid", BearerFile: "/bearer"}
		},
		"no source": func(c *Config) { c.Bindings = nil },
	} {
		c := testConfig(t)
		mutate(&c)
		if _, err := New(c, testScope().Server); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	c := testConfig(t)
	c.Bindings[0].Gateway += "/inference"
	if _, err := New(c, "https://multica.example.invalid/multica"); err != nil {
		t.Fatal("base paths of the gateway and server denied")
	}
	for _, server := range []string{"https://multica.example.invalid/multica/", "https://multica.example.invalid/a/../b"} {
		if _, err := New(testConfig(t), server); err == nil {
			t.Fatalf("server %s accepted", server)
		}
	}
	if decode([]byte(`{"version":1,"bindings":[{"workspace_id":"w","agent_id":"a","gateway":"g","key_file":"/k"}]}`), &c) == nil {
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
					target["gateway"] = "https://gateway.example.invalid/"
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

type fixtureEnv map[string]string

func (e fixtureEnv) Allowed(names []string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		if value, ok := e[name]; ok {
			out[name] = value
		}
	}
	return out
}

func TestWorkspaceEnvPrecedesConfiguredBinding(t *testing.T) {
	c := testConfig(t)
	c.WorkspaceEnv = true
	s, err := New(c, testScope().Server)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Acquire(context.Background(), testScope()); err == nil {
		t.Fatal("workspace_env without a reader accepted")
	}
	var env fixtureEnv
	supported, readErr, reads := true, error(nil), 0
	s.UseWorkspaceEnv(func(_ context.Context, workspace string) (Env, bool, error) {
		reads++
		if workspace != testScope().WorkspaceID {
			t.Error("env of another workspace read", workspace)
		}
		return env, supported, readErr
	})
	for name, tc := range map[string]struct {
		env       fixtureEnv
		supported bool
		err       error
		gateway   string
		key       string
	}{
		"env":            {fixtureEnv{EnvBaseURL: "https://env.example.invalid/inference/v1", EnvAPIKey: "sk-env\n", "OTHER": "x"}, true, nil, "https://env.example.invalid/inference", "sk-env"},
		"neither":        {fixtureEnv{"OTHER": "x"}, true, nil, testGateway, testKey},
		"unsupported":    {nil, false, nil, testGateway, testKey},
		"key only":       {fixtureEnv{EnvAPIKey: "sk-env"}, true, nil, "", ""},
		"base only":      {fixtureEnv{EnvBaseURL: "https://env.example.invalid/v1"}, true, nil, "", ""},
		"empty key":      {fixtureEnv{EnvBaseURL: "https://env.example.invalid/v1", EnvAPIKey: ""}, true, nil, "", ""},
		"without /v1":    {fixtureEnv{EnvBaseURL: "https://env.example.invalid", EnvAPIKey: "sk-env"}, true, nil, "", ""},
		"trailing slash": {fixtureEnv{EnvBaseURL: "https://env.example.invalid/v1/", EnvAPIKey: "sk-env"}, true, nil, "", ""},
		"http":           {fixtureEnv{EnvBaseURL: "http://env.example.invalid/v1", EnvAPIKey: "sk-env"}, true, nil, "", ""},
		"query":          {fixtureEnv{EnvBaseURL: "https://env.example.invalid/v1?a=1", EnvAPIKey: "sk-env"}, true, nil, "", ""},
		"bad key":        {fixtureEnv{EnvBaseURL: "https://env.example.invalid/v1", EnvAPIKey: "sk env"}, true, nil, "", ""},
		"forbidden":      {nil, false, fmt.Errorf("HTTP 403"), "", ""},
	} {
		env, supported, readErr = tc.env, tc.supported, tc.err
		target, err := s.Acquire(context.Background(), testScope())
		if tc.gateway == "" {
			if err == nil {
				t.Errorf("%s accepted", name)
			}
			continue
		}
		if err != nil || target.Gateway != tc.gateway || target.Key.Reveal() != tc.key {
			t.Errorf("%s: %v %s", name, err, target.Gateway)
		}
	}
	// Without a configured binding, a workspace without env has no connection.
	c.Bindings = nil
	if s, err = New(c, testScope().Server); err != nil {
		t.Fatal("workspace_env alone denied", err)
	}
	env, supported, readErr = fixtureEnv{}, true, nil
	s.UseWorkspaceEnv(func(context.Context, string) (Env, bool, error) { return env, supported, readErr })
	if _, err = s.Acquire(context.Background(), testScope()); err == nil {
		t.Fatal("workspace without env or binding accepted")
	}
	if reads == 0 {
		t.Fatal("workspace env never read")
	}
}

func TestDebugNamesSourceWithoutKey(t *testing.T) {
	c := testConfig(t)
	c.WorkspaceEnv = true
	s, err := New(c, testScope().Server)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	s.SetDebug(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	env := fixtureEnv{EnvBaseURL: "https://env.example.invalid/v1", EnvAPIKey: "sk-env-secret"}
	status := error(nil)
	s.UseWorkspaceEnv(func(context.Context, string) (Env, bool, error) { return env, true, status })
	_, _ = s.Acquire(context.Background(), testScope())
	env = fixtureEnv{}
	_, _ = s.Acquire(context.Background(), testScope())
	status = fmt.Errorf("Multica HTTP status 403 (response body withheld)")
	_, _ = s.Acquire(context.Background(), testScope())
	all := strings.Join(lines, "\n")
	for _, want := range []string{"source=workspace_env", "gateway=https://env.example.invalid", "source=binding", "status 403"} {
		if !strings.Contains(all, want) {
			t.Errorf("debug lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "sk-env-secret") || strings.Contains(all, testKey) {
		t.Fatal("debug exposed a key")
	}
}
