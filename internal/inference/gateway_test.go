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

// modelInfoFixture mirrors LiteLLM /v1/model/info for a virtual key, including
// deployment parameters the controller must not keep.
const modelInfoFixture = `{"data":[
{"model_name":"claude-demo","litellm_params":{"model":"anthropic/claude-demo","api_base":"http://upstream.internal","api_key":"sk-upstream"},"model_info":{"mode":"chat","litellm_provider":"anthropic","max_input_tokens":1000000,"max_output_tokens":128000,"supports_reasoning":true,"input_cost_per_token":3e-06,"output_cost_per_token":1.5e-05,"cache_read_input_token_cost":3e-07,"access_groups":["claude-pool"]}},
{"model_name":"claude-demo","litellm_params":{"model":"anthropic/claude-demo"},"model_info":{"mode":"chat","max_input_tokens":1,"max_output_tokens":1}},
{"model_name":"gpt-demo","litellm_params":{"model":"openai/gpt-demo"},"model_info":{"mode":"responses","max_input_tokens":400000,"max_output_tokens":128000,"supports_reasoning":true}},
{"model_name":"local-demo","litellm_params":{"model":"ollama/local-demo"},"model_info":{}},
{"model_name":"embed-demo","litellm_params":{"model":"openai/embed-demo"},"model_info":{"mode":"embedding","max_input_tokens":8000}},
{"model_name":"bad\nname","litellm_params":{"model":"openai/x"},"model_info":{}}
]}`

func gatewayFixture(t *testing.T, body *atomic.Value, calls *atomic.Int32) (*Service, Scope) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/inference/v1/model/info" || r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(403)
			return
		}
		b := body.Load().(string)
		if b == "" {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, b)
	}))
	t.Cleanup(server.Close)
	c := testConfig(t)
	c.AllowHTTP = true
	c.Gateways = []string{server.URL + "/inference"}
	c.Bindings[0].Gateway = c.Gateways[0]
	c.GatewayCatalog = &GatewayCatalog{TTL: "1h", DefaultModel: "claude-demo"}
	s, err := New(c, testScope().Server)
	if err != nil {
		t.Fatal(err)
	}
	return s, testScope()
}

func TestGatewayCatalogMapsWorkspaceKeyModels(t *testing.T) {
	var body atomic.Value
	var calls atomic.Int32
	body.Store(modelInfoFixture)
	s, scope := gatewayFixture(t, &body, &calls)
	c, err := s.Catalog(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 3 || c.DefaultModel != "claude-demo" {
		t.Fatalf("catalog = %+v", c)
	}
	claude, gpt, local := c.Models["claude-demo"], c.Models["gpt-demo"], c.Models["local-demo"]
	if claude.API != APIMessages || claude.Context != 1000000 || claude.Output != 128000 || claude.Thinking == nil || claude.Thinking.SupportedLevels[1].Value != "max" || claude.Cost == nil || claude.Cost.Input != 3 || claude.Cost.CacheRead != 0.3 {
		t.Fatalf("anthropic model = %+v", claude)
	}
	if gpt.API != APIResponses || gpt.Thinking == nil || gpt.Cost != nil || local.API != "" || local.Context != 128000 || local.Output != 16384 || local.Thinking != nil {
		t.Fatalf("openai/other models = %+v %+v", gpt, local)
	}
	if ModelID("claude-demo", claude) != "inference-claude/claude-demo" || ModelID("gpt-demo", gpt) != "inference-codex/gpt-demo" || ModelID("local-demo", local) != "managed-inference/local-demo" {
		t.Fatal("picker IDs do not follow the wire API")
	}
	if strings.Contains(fmt.Sprintf("%+v", c), "upstream") {
		t.Fatal("deployment parameters kept")
	}
	// Cached for the TTL; a failed refresh keeps serving the last good catalog.
	body.Store("")
	if _, err = s.Catalog(context.Background(), scope); err != nil || calls.Load() != 1 {
		t.Fatal("catalog not cached", err, calls.Load())
	}
	s.gateway.ttl = 0
	if c, err = s.Catalog(context.Background(), scope); err != nil || len(c.Models) != 3 || calls.Load() != 2 {
		t.Fatal("stale catalog not reused after a failed refresh", err)
	}
	if _, err = s.Catalog(context.Background(), Scope{Server: scope.Server, WorkspaceID: "10000000-0000-4000-8000-000000000002"}); err == nil {
		t.Fatal("workspace without a key got a catalog")
	}
}

func TestGatewayCatalogDenials(t *testing.T) {
	for name, body := range map[string]string{"outage": "", "empty": `{"data":[]}`, "malformed": `{"data":`, "embeddings only": `{"data":[{"model_name":"e","model_info":{"mode":"embedding"}}]}`} {
		var b atomic.Value
		var calls atomic.Int32
		b.Store(body)
		s, scope := gatewayFixture(t, &b, &calls)
		if _, err := s.Catalog(context.Background(), scope); err == nil {
			t.Fatalf("%s: catalog accepted", name)
		}
	}
	for name, mutate := range map[string]func(*Config){
		"with static": func(c *Config) {
			c.Catalogs = []CatalogBinding{{WorkspaceID: testScope().WorkspaceID, Catalog: testCatalog()}}
		},
		"short ttl":   func(c *Config) { c.GatewayCatalog.TTL = "1s" },
		"bad ttl":     func(c *Config) { c.GatewayCatalog.TTL = "soon" },
		"bad default": func(c *Config) { c.GatewayCatalog.DefaultModel = "a\nb" },
		"unknown api": func(c *Config) {
			c.GatewayCatalog = nil
			c.Catalogs = []CatalogBinding{{WorkspaceID: testScope().WorkspaceID, Catalog: Catalog{Models: map[string]Model{"m": {Context: 10, Output: 1, API: "embeddings"}}}}}
		},
		"negative cost": func(c *Config) {
			c.GatewayCatalog = nil
			c.Catalogs = []CatalogBinding{{WorkspaceID: testScope().WorkspaceID, Catalog: Catalog{Models: map[string]Model{"m": {Context: 10, Output: 1, Cost: &Cost{Input: -1}}}}}}
		},
	} {
		c := testConfig(t)
		c.GatewayCatalog = &GatewayCatalog{}
		mutate(&c)
		if _, err := New(c, testScope().Server); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestRelayPathsAdmitNativeModelAPIs(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens"} {
		if !RelayPath(path) {
			t.Errorf("%s denied", path)
		}
	}
	for _, path := range []string{"/v1/models", "/v1/model/info", "/key/generate", "/v1/responses/x", "/v1/embeddings", "/v1/messages/batches"} {
		if RelayPath(path) {
			t.Errorf("%s admitted", path)
		}
	}
}
