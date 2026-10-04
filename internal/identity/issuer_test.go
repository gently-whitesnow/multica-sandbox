package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

type tokenFixture struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	claims    map[string]any
	gotSecret string
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
	c.Issuers[0].URL = f.server.URL
	c.Issuers[0].TokenURL = f.server.URL + "/token"
	c.Issuers[0].JWKSURL = f.server.URL + "/keys"
	return c
}
func TestVerifiedIssuanceAndCredentialRotation(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"old-secret", "new-secret"} {
		if err = os.WriteFile(c.Bindings[0].SecretFile, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
		token, err := s.Acquire(context.Background(), ref(c), "mcp")
		if err != nil || token.Bearer() == "" || !token.ExpiresAt.After(time.Now()) || f.gotSecret != secret {
			t.Fatal("issuance or rotation failed", err)
		}
	}
	f.gotSecret = ""
	if _, err = s.Acquire(context.Background(), ref(c), "unknown"); err != ErrDenied || f.gotSecret != "" {
		t.Fatal("unapproved resource contacted issuer")
	}
}
func TestRejectInvalidAccessTokens(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	s, _ := New(c)
	changes := map[string]any{"sub": "other", "aud": "other", "iss": "https://other.example", "azp": "other", "typ": "ID", "exp": time.Now().Unix() - 10, "iat": time.Now().Unix() + 60}
	for field, value := range changes {
		t.Run(field, func(t *testing.T) {
			f.reset()
			f.claims[field] = value
			if _, err := s.Acquire(context.Background(), ref(c), "mcp"); err != ErrDenied {
				t.Fatal("invalid claim accepted")
			}
		})
	}
	f.reset()
	f.claims["iat"] = time.Now().Unix() + 20
	f.claims["exp"] = time.Now().Unix() + 10
	if _, err := s.Acquire(context.Background(), ref(c), "mcp"); err != ErrDenied {
		t.Fatal("negative token lifetime accepted")
	}
	f.reset()
	f.claims["exp"] = time.Now().Unix() + 3600
	if _, err := s.Acquire(context.Background(), ref(c), "mcp"); err != ErrDenied {
		t.Fatal("excessive lifetime accepted")
	}
	f.reset()
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.key = other
	if _, err = s.Acquire(context.Background(), ref(c), "mcp"); err != ErrDenied {
		t.Fatal("forged signature accepted")
	}
}
func TestConcurrentWorkspaceBindings(t *testing.T) {
	c := config(t)
	b := c.Bindings[0]
	b.WorkspaceID = workspaceB
	b.AgentID = agentB
	b.ClientID = "client-b"
	b.Subject = "subject-b"
	b.SecretFile = secretFile(t, "second-secret")
	c.Bindings = append(c.Bindings, b)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		for _, b := range c.Bindings {
			wg.Go(func() {
				v, e := s.Resolve(context.Background(), Ref{c.Server, b.WorkspaceID, b.AgentID})
				if e != nil || v.Subject != b.Subject {
					t.Error("cross-workspace identity")
				}
			})
		}
	}
	wg.Wait()
}
