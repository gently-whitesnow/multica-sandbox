package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestOpenBaoExchangeRotationAndCleanup(t *testing.T) {
	jwt := newTokenFixture(t)
	var secret string
	var revoked int
	var issueFailure, revokeFailure bool
	bao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["role_id"] != "client-a" || body["secret_id"] != secret || r.Header.Get("X-Vault-Token") != "" {
				t.Error("wrong login credential")
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "temporary-login"}})
		case "/identity":
			if r.Method != "GET" || r.Header.Get("X-Vault-Token") != "temporary-login" {
				t.Error("wrong identity request")
			}
			if issueFailure {
				w.WriteHeader(503)
				return
			}
			response, err := http.Get(jwt.server.URL + "/token")
			if err != nil {
				t.Error(err)
				w.WriteHeader(503)
				return
			}
			defer response.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(response.Body).Decode(&out)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": out["access_token"]}})
		case "/revoke":
			if r.Header.Get("X-Vault-Token") != "temporary-login" {
				t.Error("wrong revoke credential")
			}
			revoked++
			if revokeFailure {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer bao.Close()
	c := jwt.config(t)
	c.Issuer.TokenURL = ""
	c.TokenSource = TokenSourceConfig{Type: "openbao_approle", LoginURL: bao.URL + "/login", TokenURL: bao.URL + "/identity", RevokeURL: bao.URL + "/revoke"}
	c.Issuer.Profile, c.Issuer.PrincipalClaim, c.Issuer.Audience = "jwt", "eco_agent", "tools"
	c.Bindings[0].Principal = "executor"
	jwt.claims["eco_agent"] = "executor"
	delete(jwt.claims, "azp")
	delete(jwt.claims, "typ")
	service, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret = range []string{"first-secret", "rotated-secret"} {
		if err = os.WriteFile(c.Bindings[0].SecretFile, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
		token, e := service.Acquire(context.Background(), ref(c), nil)
		if e != nil || token.Bearer() == "" {
			t.Fatal("OpenBao JWT not accepted", e)
		}
	}
	if revoked != 2 {
		t.Fatal("login sessions not revoked")
	}
	issueFailure = true
	if _, err = service.Acquire(context.Background(), ref(c), nil); err != ErrDenied || revoked != 3 {
		t.Fatal("failed issuance not cleaned")
	}
	issueFailure, revokeFailure = false, true
	if _, err = service.Acquire(context.Background(), ref(c), nil); err != ErrDenied {
		t.Fatal("token released after failed login revocation")
	}
}

func TestExplicitJWTProfile(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	c.Issuer.Profile, c.Issuer.PrincipalClaim, c.Issuer.Audience = "jwt", "eco_agent", "tools"
	c.Bindings[0].Principal = "executor"
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	reset := func() {
		f.reset()
		f.claims["eco_agent"] = "executor"
		delete(f.claims, "azp")
		delete(f.claims, "typ")
	}
	reset()
	if _, err = s.Acquire(context.Background(), ref(c), nil); err != nil {
		t.Fatal("JWT profile incorrectly requires Keycloak claims", err)
	}
	for _, change := range []func(){
		func() { f.claims["aud"] = "wrong-audience" },
		func() { delete(f.claims, "aud") },
		func() { f.claims["eco_agent"] = "reviewer" },
		func() { f.claims["eco_agent"] = []string{"executor"} },
		func() { delete(f.claims, "sub") },
		func() { f.claims["nbf"] = f.claims["exp"] },
		func() { f.claims["nbf"] = "invalid" },
	} {
		reset()
		change()
		if _, err = s.Acquire(context.Background(), ref(c), nil); err != ErrDenied {
			t.Fatal("invalid explicit profile accepted")
		}
	}
}

func TestSourceAndProfileConfiguration(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Issuer.Profile = "unverified" },
		func(c *Config) { c.Issuer.Profile = "jwt" },
		func(c *Config) {
			c.Issuer.Profile = "jwt"
			c.Issuer.Audience = "tools"
			c.Issuer.PrincipalClaim = "sub"
		},
		func(c *Config) { c.TokenSource.Type = "opaque" },
		func(c *Config) {
			c.TokenSource = TokenSourceConfig{Type: "oauth2_client_credentials", TokenURL: "https://different.example/token"}
		},
		func(c *Config) {
			c.Issuer.TokenURL = ""
			c.TokenSource = TokenSourceConfig{Type: "openbao_approle", TokenURL: "https://one.example/token", LoginURL: "https://two.example/login", RevokeURL: "https://two.example/revoke"}
		},
	} {
		c := config(t)
		mutate(&c)
		if _, err := New(c, identityTestServer); err != ErrDenied {
			t.Fatal("invalid source/profile allowed")
		}
	}
}

func TestOpenBaoCancellationStillRevokesLogin(t *testing.T) {
	issued := make(chan struct{})
	revoked := make(chan struct{}, 1)
	bao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login":
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "temporary-login"}})
		case "/identity":
			close(issued)
			<-r.Context().Done()
		case "/revoke":
			revoked <- struct{}{}
			w.WriteHeader(204)
		}
	}))
	defer bao.Close()
	source, err := newTokenSource(TokenSourceConfig{Type: "openbao_approle", LoginURL: bao.URL + "/login", TokenURL: bao.URL + "/identity", RevokeURL: bao.URL + "/revoke"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := source.Token(ctx, Credentials{ClientID: "client", secret: "secret"}); done <- e }()
	<-issued
	cancel()
	if err = <-done; err != ErrDenied {
		t.Fatal("cancelled issuance accepted")
	}
	select {
	case <-revoked:
	default:
		t.Fatal("cancelled issuance left login token active")
	}
}
