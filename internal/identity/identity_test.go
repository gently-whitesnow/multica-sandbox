package identity

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
	"time"
)

const workspaceA = "10000000-0000-4000-8000-000000000001"
const workspaceB = "10000000-0000-4000-8000-000000000002"
const agentA = "20000000-0000-4000-8000-000000000001"
const agentB = "20000000-0000-4000-8000-000000000002"

func secretFile(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func config(t *testing.T) Config {
	return Config{Version: 1, Server: "https://multica.example", AllowHTTP: true, Issuers: []IssuerConfig{{Name: "corp", URL: "https://issuer.example/realm", TokenURL: "https://issuer.example/token", JWKSURL: "https://issuer.example/keys", Resources: map[string]Resource{"mcp": {Audience: "tools", MaxTTLSeconds: 300}}}}, Bindings: []Binding{{WorkspaceID: workspaceA, AgentID: agentA, Principal: Principal{"corp", "client-a", "subject-a"}, SecretFile: secretFile(t, "private-sentinel")}}}
}
func ref(c Config) Ref { return Ref{c.Server, workspaceA, agentA} }
func TestStaticBindingIsolationAndRotation(t *testing.T) {
	c := config(t)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Resolve(context.Background(), ref(c))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Ref{{c.Server, workspaceB, agentA}, {c.Server, workspaceA, agentB}, {"https://other.example", workspaceA, agentA}} {
		if _, err = s.Resolve(context.Background(), r); err == nil {
			t.Fatal("cross-binding resolution allowed")
		}
	}
	if err = os.WriteFile(c.Bindings[0].SecretFile, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := s.Resolve(context.Background(), ref(c))
	if err != nil || next.secret == first.secret {
		t.Fatal("secret not re-read")
	}
	for _, value := range []any{first, AccessToken{value: "private-sentinel"}} {
		if strings.Contains(fmt.Sprintf("%v %+v %#v", value, value, value), "private-sentinel") {
			t.Fatal("format exposed secret")
		}
		if _, err = json.Marshal(value); err == nil {
			t.Fatal("credentials serializable")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.Acquire(ctx, ref(c), "mcp"); err == nil {
		t.Fatal("cancel ignored")
	}
}
func TestInvalidConfiguration(t *testing.T) {
	tests := map[string]func(*Config){
		"duplicate binding": func(c *Config) { c.Bindings = append(c.Bindings, c.Bindings[0]) },
		"duplicate issuer":  func(c *Config) { c.Issuers = append(c.Issuers, c.Issuers[0]) },
		"issuer aliases":    func(c *Config) { other := c.Issuers[0]; other.Name = "alias"; c.Issuers = append(c.Issuers, other) },
		"agent has two clients": func(c *Config) {
			b := c.Bindings[0]
			b.WorkspaceID = workspaceB
			b.ClientID = "other-client"
			b.Subject = "other-subject"
			c.Bindings = append(c.Bindings, b)
		},
		"missing source":    func(c *Config) { c.Bindings = nil },
		"two sources":       func(c *Config) { c.External = &ExternalConfig{URL: "https://resolver.example", BearerFile: "/key"} },
		"unknown issuer":    func(c *Config) { c.Bindings[0].Issuer = "unknown" },
		"relative secret":   func(c *Config) { c.Bindings[0].SecretFile = "secret" },
		"shared client":     func(c *Config) { b := c.Bindings[0]; b.AgentID = agentB; c.Bindings = append(c.Bindings, b) },
		"unbounded ttl":     func(c *Config) { c.Issuers[0].Resources["mcp"] = Resource{Audience: "tools", MaxTTLSeconds: 99999} },
		"invalid server":    func(c *Config) { c.Server = "https://multica.example/path" },
		"url credentials":   func(c *Config) { c.Issuers[0].TokenURL = "https://secret@issuer.example/token" },
		"insecure endpoint": func(c *Config) { c.AllowHTTP = false; c.Issuers[0].JWKSURL = "http://issuer.example/keys" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			c := config(t)
			change(&c)
			if _, err := New(c); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestRemoteResolverContract(t *testing.T) {
	c := config(t)
	r := ref(c)
	response := resolveResponse{1, r, c.Bindings[0].Principal, "private-sentinel"}
	bearer := secretFile(t, "resolver-a")
	expectedBearer := "resolver-a"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var request resolveRequest
		if q.Method != "POST" || q.Header.Get("Authorization") != "Bearer "+expectedBearer || json.NewDecoder(q.Body).Decode(&request) != nil || request.Version != 1 || request.Agent != r {
			w.WriteHeader(403)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	c.Bindings = nil
	c.External = &ExternalConfig{server.URL, bearer}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-sentinel", "rotated"} {
		response.ClientSecret = secret
		got, err := s.Resolve(context.Background(), r)
		if err != nil || got.secret != secret {
			t.Fatal("external binding not refreshed")
		}
	}
	expectedBearer = "resolver-b"
	if err = os.WriteFile(bearer, []byte(expectedBearer), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Resolve(context.Background(), r); err != nil {
		t.Fatal("resolver credential not refreshed")
	}
	response.Agent.WorkspaceID = workspaceB
	if _, err = s.Resolve(context.Background(), r); err == nil {
		t.Fatal("mismatched binding accepted")
	}
	response.Agent = r
	response.Issuer = "unapproved"
	if _, err = s.Resolve(context.Background(), r); err == nil {
		t.Fatal("unapproved issuer accepted")
	}
}
func TestResolverFailuresDoNotLeak(t *testing.T) {
	for _, body := range []string{"private-sentinel", strings.Repeat("private-sentinel", 6000), `{"version":2}`, `{"version":1,"unexpected":"private-sentinel"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		c := config(t)
		c.Bindings = nil
		c.External = &ExternalConfig{server.URL, secretFile(t, "bearer")}
		s, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Resolve(context.Background(), ref(c))
		server.Close()
		if err != ErrDenied {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	c := config(t)
	c.Bindings = nil
	c.External = &ExternalConfig{redirect.URL, secretFile(t, "bearer")}
	s, _ := New(c)
	if _, err := s.Resolve(context.Background(), ref(c)); err != ErrDenied || hits != 0 {
		t.Fatal("redirect followed")
	}
	redirect.Close()
	if _, err := s.Resolve(context.Background(), ref(c)); err != ErrDenied {
		t.Fatal("outage not denied")
	}
}
func TestConfigAndSecretBounds(t *testing.T) {
	c := config(t)
	data, _ := json.Marshal(c)
	p := secretFile(t, string(data))
	if _, err := ReadConfig(p); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{string(data) + "{}", `{"version":1,"secret":"private-sentinel"}`, strings.Repeat("x", maxDocument+1)} {
		if err := os.WriteFile(p, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(p); err != ErrDenied {
			t.Fatal("invalid document accepted")
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 8193), "bad\nvalue"} {
		if _, err := readSecret(secretFile(t, bad)); err != ErrDenied {
			t.Fatal("invalid secret accepted")
		}
	}
	if _, err := readSecret(t.TempDir()); err != ErrDenied {
		t.Fatal("directory accepted")
	}
}

func TestRemoteContextCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	defer server.Close()
	defer close(release)
	c := config(t)
	c.Bindings = nil
	c.External = &ExternalConfig{server.URL, secretFile(t, "private-sentinel")}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Resolve(ctx, ref(c)); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != ErrDenied {
			t.Fatal("cancellation failed or leaked transport error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not honor cancellation")
	}
}
