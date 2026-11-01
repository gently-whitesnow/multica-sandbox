package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCLIProxyCatalogFetch(t *testing.T) {
	var calls atomic.Int32
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testKey || r.URL.Path != "/gateway/v1/models" {
			t.Error("catalog request escaped the binding or lost its key")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Has("client_version") {
			if r.URL.RawQuery != "client_version=" {
				t.Error("catalog invented a client version")
			}
			fmt.Fprint(w, `{"models":[{"slug":"pool/gpt-fixture","display_name":"Fixture","context_window":500000,"max_context_window":900000,"max_tokens":100000,"supported_in_api":true,"visibility":"list","base_instructions":"must not be projected"}]}`)
		} else {
			fmt.Fprint(w, `{"data":[{"id":"pool/gpt-fixture","owned_by":"openai"}]}`)
		}
	}))
	defer server.Close()
	config := testConfig(t)
	config.AllowHTTP = true
	config.Bindings[0].Gateway = server.URL + "/gateway"
	config.GatewayCatalog = &GatewayCatalog{Format: "cliproxy", TTL: "1h", DefaultModel: "pool/gpt-fixture"}
	service, err := New(config, testScope().Server)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := service.Catalog(context.Background(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	model := catalog.Models["pool/gpt-fixture"]
	if model.Context != 500000 || model.Output != 100000 || model.API != APIResponses || catalog.DefaultModel != "pool/gpt-fixture" || calls.Load() != 2 {
		t.Fatalf("catalog did not preserve gateway metadata: %+v, calls=%d", catalog, calls.Load())
	}
	if strings.Contains(fmt.Sprint(catalog), "must not be projected") {
		t.Fatal("gateway instructions kept")
	}
	unavailable.Store(true)
	if _, err = service.Catalog(context.Background(), testScope()); err != nil || calls.Load() != 2 {
		t.Fatal("catalog not cached")
	}
	service.gateway.ttl = 0
	if _, err = service.Catalog(context.Background(), testScope()); err != nil || calls.Load() != 3 {
		t.Fatal("last good catalog lost during outage")
	}
	if _, err = service.Catalog(context.Background(), Scope{Server: testScope().Server, WorkspaceID: "10000000-0000-4000-8000-000000000002"}); err == nil || calls.Load() != 3 {
		t.Fatal("unbound workspace reached catalog")
	}
}

func TestCLIProxyCatalogRefusesMetadataRedirect(t *testing.T) {
	var escaped atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { escaped.Add(1); w.WriteHeader(200) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("client_version") {
			http.Redirect(w, r, other.URL, http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"fixture","owned_by":"openai"}]}`)
	}))
	defer server.Close()
	config := testConfig(t)
	config.AllowHTTP = true
	config.Bindings[0].Gateway = server.URL
	config.GatewayCatalog = &GatewayCatalog{Format: "cliproxy"}
	service, err := New(config, testScope().Server)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Catalog(context.Background(), testScope()); err == nil || escaped.Load() != 0 {
		t.Fatal("metadata redirect followed or accepted")
	}
}
