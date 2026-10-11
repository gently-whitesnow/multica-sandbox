package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

type catalogInference struct {
	workspaceInference
	catalog      inference.Catalog
	err          error
	scope        inference.Scope
	catalogCalls int
}

func (s *catalogInference) Catalog(_ context.Context, scope inference.Scope) (inference.Catalog, error) {
	s.scope = scope
	s.catalogCalls++
	return s.catalog, s.err
}
func TestModelSelectionIsIntentNotCatalogAuthorization(t *testing.T) {
	for _, mode := range []string{"default", "explicit", "stale", "outage", "no-reasoning", "new-effort", "no-default", "bad-model", "empty-qualified-model", "bad-effort"} {
		t.Run(mode, func(t *testing.T) {
			source := &catalogInference{catalog: inference.Catalog{DefaultModel: "demo", Models: map[string]inference.Model{"demo": {Context: 64000, Output: 4096, Thinking: &inference.Thinking{DefaultLevel: "medium", SupportedLevels: []inference.ThinkingLevel{{Value: "medium", Label: "Medium"}}}}}}}
			task := safeTask()
			wantModel, wantEffort := "demo", "medium"
			switch mode {
			case "explicit":
				task.Agent.Model = "managed-inference/demo"
				task.Agent.ThinkingLevel = "high"
				wantEffort = "high"
			case "stale":
				task.Agent.Model = "new-model"
				wantModel = "new-model"
				wantEffort = ""
			case "outage":
				task.Agent.Model = "new-model"
				task.Agent.ThinkingLevel = "high"
				source.catalog = inference.Catalog{}
				source.err = errors.New("unavailable")
				wantModel, wantEffort = "new-model", "high"
			case "no-reasoning":
				source.catalog.Models["demo"] = inference.Model{Context: 64000, Output: 4096}
				wantEffort = ""
			case "new-effort":
				task.Agent.ThinkingLevel = "deep"
				wantEffort = "deep"
			case "no-default":
				source.catalog.DefaultModel = ""
			case "empty-qualified-model":
				task.Agent.Model = "managed-inference/"
			case "bad-model":
				task.Agent.Model = "bad\nmodel"
			case "bad-effort":
				task.Agent.ThinkingLevel = "high\n"
			}
			r := &running{adapter: &Adapter{Server: "https://controller.example.invalid", Inference: source}, task: task}
			got, err := r.selectInference(context.Background())
			denied := mode == "no-default" || mode == "bad-model" || mode == "empty-qualified-model" || mode == "bad-effort"
			if denied {
				if err == nil {
					t.Fatal("unmappable selection accepted")
				}
				return
			}
			if err != nil || got.model != wantModel || got.effort != wantEffort {
				t.Fatalf("selection = %+v, %v", got, err)
			}
			if source.scope != (inference.Scope{Server: r.adapter.Server, WorkspaceID: task.WorkspaceID}) {
				t.Fatal("catalog selectors did not come from trusted controller/claim")
			}

		})
	}
}

func TestWireAPISelectsNativeProvider(t *testing.T) {
	high := &inference.Thinking{SupportedLevels: []inference.ThinkingLevel{{Value: "high", Label: "High"}, {Value: "max", Label: "Max"}}}
	catalog := inference.Catalog{DefaultModel: "claude-demo", Models: map[string]inference.Model{
		"claude-demo": {API: inference.APIMessages, Context: 200000, Output: 64000, Thinking: high, Cost: &inference.Cost{Input: 3, Output: 15, CacheRead: 0.3}},
		"gpt-demo":    {API: inference.APIResponses, Context: 400000, Output: 128000},
		"other":       {Context: 8000, Output: 1000},
	}}
	for _, test := range []struct{ model, effort, provider, npm, name string }{
		{"", "", "inference-claude", "@ai-sdk/anthropic", "claude-demo"},
		{"claude-demo", "max", "inference-claude", "@ai-sdk/anthropic", "claude-demo"},
		{"inference-codex/gpt-demo", "high", "inference-codex", "@ai-sdk/openai", "gpt-demo"},
		// Legacy selections keep the chat provider even for a model the catalog serves natively.
		{"managed-inference/claude-demo", "", "managed-inference", "@ai-sdk/openai-compatible", "claude-demo"},
		{"inference-claude/newer-claude", "", "inference-claude", "@ai-sdk/anthropic", "newer-claude"},
		{"other", "", "managed-inference", "@ai-sdk/openai-compatible", "other"},
	} {
		task := safeTask()
		task.Agent.Model, task.Agent.ThinkingLevel = test.model, test.effort
		r := &running{adapter: &Adapter{Server: "https://controller.example.invalid", Inference: &catalogInference{catalog: catalog}, InferenceRelayURL: "http://inference-relay:8092"}, task: task, inferenceToken: inference.RelayPrefix + "opaque"}
		data, err := r.configureInference(context.Background(), []byte(`{"permission":{"*":"deny"}}`))
		if err != nil {
			t.Fatalf("%s: %v", test.model, err)
		}
		var config struct {
			Model    string   `json:"model"`
			Enabled  []string `json:"enabled_providers"`
			Provider map[string]struct {
				NPM     string                    `json:"npm"`
				Options map[string]string         `json:"options"`
				Models  map[string]map[string]any `json:"models"`
			} `json:"provider"`
			Permission map[string]string `json:"permission"`
		}
		if json.Unmarshal(data, &config) != nil || config.Model != test.provider+"/"+test.name || r.model != config.Model || len(config.Enabled) != 1 || config.Enabled[0] != test.provider || len(config.Provider) != 1 || config.Permission["*"] != "deny" {
			t.Fatalf("%s: config %s", test.model, data)
		}
		p := config.Provider[test.provider]
		if p.NPM != test.npm || p.Options["baseURL"] != "http://inference-relay:8092/v1" || p.Options["apiKey"] != inference.RelayPrefix+"opaque" || len(p.Models) != 1 || p.Models[test.name] == nil {
			t.Fatalf("%s: provider %s", test.model, data)
		}
		if test.name == "claude-demo" && test.provider == "inference-claude" {
			variant, _ := json.Marshal(p.Models[test.name]["variants"])
			cost, _ := json.Marshal(p.Models[test.name]["cost"])
			if string(variant) != `{"high":{"thinking":{"budgetTokens":16000,"type":"enabled"}},"max":{"thinking":{"budgetTokens":31999,"type":"enabled"}}}` || string(cost) != `{"cache_read":0.3,"input":3,"output":15}` {
				t.Fatalf("messages variants/cost: %s %s", variant, cost)
			}
		}
	}
	task := safeTask()
	task.Agent.Model, task.Agent.ThinkingLevel = "inference-claude/claude-demo", "deep"
	r := &running{adapter: &Adapter{Server: "https://controller.example.invalid", Inference: &catalogInference{catalog: catalog}}, task: task}
	if _, err := r.selectInference(context.Background()); err == nil {
		t.Fatal("unmappable Messages reasoning option accepted")
	}
}
