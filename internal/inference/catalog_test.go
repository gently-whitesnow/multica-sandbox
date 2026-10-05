package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

func testCatalog() Catalog {
	return Catalog{DefaultModel: "demo", Models: map[string]Model{"demo": {Context: 64000, Output: 4096, Thinking: &Thinking{DefaultLevel: "high", SupportedLevels: []ThinkingLevel{{Value: "high", Label: "High"}}}}}}
}
func TestCatalogIsolationCopiesAndSeparateIdentity(t *testing.T) {
	c := testConfig()
	ref := testRef()
	issuer := &testIssuer{}
	c.Catalogs = []CatalogBinding{{WorkspaceID: ref.WorkspaceID, AgentID: ref.AgentID, Catalog: testCatalog()}}
	s, err := New(c, ref.Server, issuer)
	if err != nil {
		t.Fatal(err)
	}
	c.Catalogs[0].Models["demo"].Thinking.SupportedLevels[0].Value = "changed"
	got, err := s.Catalog(context.Background(), ref)
	if err != nil || got.Models["demo"].Thinking.SupportedLevels[0].Value != "high" || issuer.calls != 0 {
		t.Fatal("catalog changed or issued identity", err)
	}
	got.Models["demo"].Thinking.SupportedLevels[0].Value = "changed"
	got, err = s.Catalog(context.Background(), ref)
	if err != nil || got.Models["demo"].Thinking.SupportedLevels[0].Value != "high" {
		t.Fatal("catalog response leaked shared state", err)
	}
	ref.AgentID = "20000000-0000-4000-8000-000000000002"
	if _, err = s.Catalog(context.Background(), ref); err == nil {
		t.Fatal("cross-agent catalog accepted")
	}
	if _, err = s.Acquire(context.Background(), testRef()); err != nil || issuer.calls != 1 {
		t.Fatal("identity required a selected model", err)
	}
}
func TestExternalCatalogContract(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "bearer")
	if err := os.WriteFile(secret, []byte("catalog-admin"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "spoof", "credentials", "outage", "redirect", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Version int          `json:"version"`
					Agent   identity.Ref `json:"agent"`
				}
				if r.Header.Get("Authorization") != "Bearer catalog-admin" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Agent != testRef() {
					t.Error("unbound catalog request")
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
					req.Agent.AgentID = "20000000-0000-4000-8000-000000000002"
				}
				response := map[string]any{"version": 1, "agent": req.Agent, "catalog": testCatalog()}
				if mode == "credentials" {
					response["api_key"] = "forbidden"
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			c := testConfig()
			c.AllowHTTP = true
			c.CatalogExternal = &identity.ExternalConfig{URL: server.URL, BearerFile: secret}
			issuer := &testIssuer{}
			s, err := New(c, testRef().Server, issuer)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Catalog(context.Background(), testRef())
			if (err == nil) != (mode == "valid") || issuer.calls != 0 {
				t.Fatal("catalog boundary failed", err)
			}
			if _, err = s.Acquire(context.Background(), testRef()); err != nil || issuer.calls != 1 {
				t.Fatal("catalog outage denied independent identity", err)
			}
		})
	}
}
