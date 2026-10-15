package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

type tokenFixture struct {
	server      *httptest.Server
	key         *rsa.PrivateKey
	claims      map[string]any
	gotSecret   string
	gotScope    string
	gotResource string
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &tokenFixture{key: key}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/keys" {
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
			return
		}
		_ = r.ParseForm()
		f.gotSecret = r.Form.Get("client_secret")
		f.gotScope = r.Form.Get("scope")
		f.gotResource = r.Form.Get("resource")
		signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		payload, _ := json.Marshal(f.claims)
		signed, e := signer.Sign(payload)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		raw, _ := signed.CompactSerialize()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": raw, "token_type": "Bearer", "expires_in": 120})
	}))
	t.Cleanup(f.server.Close)
	f.reset()
	return f
}
func (f *tokenFixture) reset() {
	now := time.Now().Unix()
	f.claims = map[string]any{"iss": f.server.URL, "sub": "subject-a", "aud": "tools", "azp": "client-a", "typ": "Bearer", "iat": now, "exp": now + 120}
}
func (f *tokenFixture) config(t *testing.T) Config {
	c := config(t)
	c.Issuer.URL = f.server.URL
	c.Issuer.TokenURL = f.server.URL + "/token"
	c.Issuer.JWKSURL = f.server.URL + "/keys"
	return c
}
func TestVerifiedIssuanceAndCredentialRotation(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"old-secret", "new-secret"} {
		if err = os.WriteFile(c.Bindings[0].SecretFile, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
		token, err := s.Acquire(context.Background(), ref(c), nil)
		if err != nil || token.Bearer() == "" || !token.ExpiresAt.After(time.Now()) || f.gotSecret != secret {
			t.Fatal("issuance or rotation failed", err)
		}
	}
	f.gotSecret = ""
	if _, err = s.AcquireForMCP(context.Background(), ref(c), nil, "https://unapproved.example/mcp"); err != ErrDenied || f.gotSecret != "" {
		t.Fatal("unapproved MCP contacted issuer")
	}
}
func TestRejectInvalidAccessTokens(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	s, _ := New(c, identityTestServer)
	changes := map[string]any{"iss": "https://other.example", "azp": "other", "typ": "ID", "exp": time.Now().Unix() - 10, "iat": time.Now().Unix() + 60}
	for field, value := range changes {
		t.Run(field, func(t *testing.T) {
			f.reset()
			f.claims[field] = value
			if _, err := s.Acquire(context.Background(), ref(c), nil); err != ErrDenied {
				t.Fatal("invalid claim accepted")
			}
		})
	}
	f.reset()
	f.claims["iat"] = time.Now().Unix() + 20
	f.claims["exp"] = time.Now().Unix() + 10
	if _, err := s.Acquire(context.Background(), ref(c), nil); err != ErrDenied {
		t.Fatal("negative token lifetime accepted")
	}
	f.reset()
	f.claims["exp"] = time.Now().Unix() + 3600
	if _, err := s.Acquire(context.Background(), ref(c), nil); err != ErrDenied {
		t.Fatal("excessive lifetime accepted")
	}
	f.reset()
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.key = other
	if _, err = s.Acquire(context.Background(), ref(c), nil); err != ErrDenied {
		t.Fatal("forged signature accepted")
	}
}
func TestConcurrentWorkspaceBindings(t *testing.T) {
	c := config(t)
	b := c.Bindings[0]
	b.WorkspaceID = workspaceB
	b.AgentID = agentB
	b.ClientID = "client-b"
	b.SecretFile = secretFile(t, "second-secret")
	c.Bindings = append(c.Bindings, b)
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		for _, b := range c.Bindings {
			wg.Go(func() {
				v, e := s.Resolve(context.Background(), Ref{identityTestServer, b.WorkspaceID, b.AgentID}, nil)
				if e != nil || v.ClientID != b.ClientID {
					t.Error("cross-workspace identity")
				}
			})
		}
	}
	wg.Wait()
}

func TestMCPDeliveryAndIAMParameters(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	c.MCP = []string{"https://tools.example/mcp"}
	c.Bindings[0].Token = TokenRequest{Scopes: []string{"profile"}, Resource: "https://tools.example"}
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"https://other.example/mcp", "https://tools.example/mcp/", "https://tools.example/mcp?next=other", "https://tools.example/mcp/extra"} {
		if _, err = s.AcquireForMCP(context.Background(), ref(c), nil, target); err != ErrDenied || f.gotSecret != "" {
			t.Fatal("unapproved destination contacted issuer")
		}
	}
	c.Bindings[0].Token.Scopes[0] = "mutated"
	for _, aud := range []any{"another-recipient", nil} {
		f.reset()
		if aud == nil {
			delete(f.claims, "aud")
		} else {
			f.claims["aud"] = aud
		}
		f.claims["realm_access"] = map[string]any{"roles": []string{"arbitrary-role"}}
		if _, err = s.AcquireForMCP(context.Background(), ref(c), nil, "https://tools.example/mcp"); err != nil || f.gotScope != "profile" || f.gotResource != "https://tools.example" {
			t.Fatal("delivery depends on recipient permissions or lost IAM parameters", err)
		}
	}
}

type agentEnv map[string]string

func (e agentEnv) Allowed(names []string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		if value, ok := e[name]; ok {
			out[name] = value
		}
	}
	return out
}

func TestAgentEnvPrecedesConfiguredBinding(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	env := agentEnv{EnvClientID: "client-env", EnvClientSecret: "env-secret", "OTHER": "x"}
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Resolve(context.Background(), ref(c), env); err != nil || got.ClientID != "client-a" {
		t.Fatal("agent env used without agent_env", err)
	}
	c.AgentEnv = true
	if s, err = New(c, identityTestServer); err != nil {
		t.Fatal(err)
	}
	f.claims["azp"] = "client-env"
	token, err := s.Acquire(context.Background(), ref(c), env)
	if err != nil || token.Bearer() == "" || f.gotSecret != "env-secret" {
		t.Fatal("agent env credentials not issued", err)
	}
	f.claims["azp"] = "client-a"
	for _, fallback := range []Env{nil, agentEnv{"OTHER": "x"}} {
		if got, err := s.Resolve(context.Background(), ref(c), fallback); err != nil || got.ClientID != "client-a" {
			t.Fatal("agent without env did not fall back to its binding", err)
		}
	}
	for _, bad := range []agentEnv{{EnvClientID: "client-env"}, {EnvClientSecret: "env-secret"}, {EnvClientID: "client-env", EnvClientSecret: ""}, {EnvClientID: "a\nb", EnvClientSecret: "s"}} {
		if _, err = s.Resolve(context.Background(), ref(c), bad); err != ErrDenied {
			t.Fatal("incomplete agent env accepted", bad)
		}
	}
	// agent_env alone: an agent without env has no identity.
	c.Bindings = nil
	if s, err = New(c, identityTestServer); err != nil {
		t.Fatal("agent_env alone denied", err)
	}
	if _, err = s.Resolve(context.Background(), ref(c), agentEnv{}); err != ErrDenied {
		t.Fatal("agent without env or binding resolved")
	}
	if _, err = s.Resolve(context.Background(), Ref{"https://other.example", workspaceA, agentA}, env); err != ErrDenied {
		t.Fatal("agent env accepted for another server")
	}
}

func TestDebugNamesReasonsWithoutSecrets(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	c.AgentEnv = true
	c.MCP = []string{"https://tools.example/mcp"}
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	s.SetDebug(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	env := agentEnv{EnvClientID: "client-env", EnvClientSecret: "env-secret"}
	f.claims["azp"] = "client-env"
	token, err := s.AcquireForMCP(context.Background(), ref(c), env, "https://tools.example/mcp")
	if err != nil {
		t.Fatal(err)
	}
	f.claims["azp"] = "someone-else"
	if _, err = s.AcquireForMCP(context.Background(), ref(c), env, "https://tools.example/mcp"); err != ErrDenied {
		t.Fatal("foreign azp accepted")
	}
	_, _ = s.AcquireForMCP(context.Background(), ref(c), env, "https://other.example/mcp")
	all := strings.Join(lines, "\n")
	for _, want := range []string{"source=agent_env", "client=client-env", "token issued", `azp "someone-else"`, "not in the mcp list"} {
		if !strings.Contains(all, want) {
			t.Errorf("debug lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "env-secret") || strings.Contains(all, token.Bearer()) {
		t.Fatal("debug exposed a secret")
	}
}
