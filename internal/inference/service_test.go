package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

type testIssuer struct {
	calls    int
	expected string
}

func (i *testIssuer) AcquireForIssuer(_ context.Context, _ identity.Ref, expected string) (identity.AccessToken, error) {
	i.calls++
	i.expected = expected
	return identity.AccessToken{ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func testRef() identity.Ref {
	return identity.Ref{Server: "https://multica.example.invalid", WorkspaceID: "10000000-0000-4000-8000-000000000001", AgentID: "20000000-0000-4000-8000-000000000001"}
}
func testTarget() Target {
	return Target{Gateway: Gateway{URL: "https://gateway.example.invalid/v1", Issuer: "inference"}}
}
func testConfig() Config {
	r := testRef()
	return Config{Version: 1, Gateways: []Gateway{testTarget().Gateway}, Bindings: []Binding{{WorkspaceID: r.WorkspaceID, AgentID: r.AgentID, Target: testTarget()}}}
}
func TestStaticSelectorsAndCopies(t *testing.T) {
	issuer := &testIssuer{}
	config := testConfig()
	s, err := New(config, testRef().Server, issuer)
	if err != nil {
		t.Fatal(err)
	}
	config.Bindings[0].URL = "https://changed.example.invalid"
	session, err := s.Acquire(context.Background(), testRef())
	if err != nil || issuer.expected != "inference" || session.URL != testTarget().URL {
		t.Fatal("binding not preserved", err)
	}
	session.URL = "https://changed.example.invalid"
	ref := testRef()
	ref.WorkspaceID = "10000000-0000-4000-8000-000000000002"
	if _, err = s.Acquire(context.Background(), ref); err == nil || issuer.calls != 1 {
		t.Fatal("cross-workspace issued identity")
	}
	ref = testRef()
	ref.Server = "https://other.example.invalid"
	if _, err = s.Acquire(context.Background(), ref); err == nil {
		t.Fatal("cross-server accepted")
	}
}
func TestExternalBindingFailClosedBeforeIssuance(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "resolver")
	if err := os.WriteFile(secret, []byte("fixture-admin"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "spoof", "recipient", "issuer", "credentials", "oversize", "outage", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			target := testTarget()
			ref := testRef()
			issuer := &testIssuer{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Version int          `json:"version"`
					Agent   identity.Ref `json:"agent"`
				}
				if r.Header.Get("Authorization") != "Bearer fixture-admin" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Agent != testRef() {
					t.Error("resolver request not authenticated/bound")
					w.WriteHeader(403)
					return
				}
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
					ref.WorkspaceID = "10000000-0000-4000-8000-000000000002"
				case "recipient":
					target.URL = "https://unapproved.example.invalid/v1"
				case "issuer":
					target.Issuer = "mcp"
				}
				body := map[string]any{"version": 1, "agent": ref, "target": target}
				if mode == "credentials" {
					body["api_key"] = "forbidden"
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			c := testConfig()
			c.Bindings = nil
			c.AllowHTTP = true
			c.External = &identity.ExternalConfig{URL: server.URL, BearerFile: secret}
			s, err := New(c, testRef().Server, issuer)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Acquire(context.Background(), testRef())
			if mode == "valid" {
				if err != nil || issuer.calls != 1 {
					t.Fatal("valid resolution failed", err)
				}
			} else if err == nil || issuer.calls != 0 {
				t.Fatal("unsafe response issued identity", err)
			}
		})
	}
}
