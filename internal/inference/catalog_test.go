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
func TestCatalogIsolationCopiesAndSeparateBinding(t *testing.T) {
	c := testConfig(t)
	scope := testScope()
	c.Catalogs = []CatalogBinding{{WorkspaceID: scope.WorkspaceID, Catalog: testCatalog()}}
	s, err := New(c, scope.Server)
	if err != nil {
		t.Fatal(err)
	}
	c.Catalogs[0].Models["demo"].Thinking.SupportedLevels[0].Value = "changed"
	got, err := s.Catalog(context.Background(), scope)
	if err != nil || got.Models["demo"].Thinking.SupportedLevels[0].Value != "high" {
		t.Fatal("catalog changed", err)
	}
	got.Models["demo"].Thinking.SupportedLevels[0].Value = "changed"
	got, err = s.Catalog(context.Background(), scope)
	if err != nil || got.Models["demo"].Thinking.SupportedLevels[0].Value != "high" {
		t.Fatal("catalog response leaked shared state", err)
	}
	for _, other := range []Scope{{Server: scope.Server, WorkspaceID: "10000000-0000-4000-8000-000000000002"}, {Server: "https://other.example.invalid", WorkspaceID: scope.WorkspaceID}, {Server: scope.Server}} {
		if _, err = s.Catalog(context.Background(), other); err == nil {
			t.Fatal("cross-workspace or cross-server catalog accepted", other)
		}
	}
	if _, err = s.Acquire(context.Background(), scope); err != nil {
		t.Fatal("binding required a selected model", err)
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
					Version   int   `json:"version"`
					Workspace Scope `json:"workspace"`
				}
				if r.Header.Get("Authorization") != "Bearer catalog-admin" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Workspace != testScope() {
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
					req.Workspace.WorkspaceID = "10000000-0000-4000-8000-000000000002"
				}
				response := map[string]any{"version": 1, "workspace": req.Workspace, "catalog": testCatalog()}
				if mode == "credentials" {
					response["api_key"] = "forbidden"
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			c := testConfig(t)
			c.AllowHTTP = true
			c.CatalogExternal = &identity.ExternalConfig{URL: server.URL, BearerFile: secret}
			s, err := New(c, testScope().Server)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Catalog(context.Background(), testScope())
			if (err == nil) != (mode == "valid") {
				t.Fatal("catalog boundary failed", err)
			}
			if _, err = s.Acquire(context.Background(), testScope()); err != nil {
				t.Fatal("catalog outage denied the independent binding", err)
			}
		})
	}
}
