package inference

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// gatewayModels bounds a discovered catalog; the Multica picker and OpenCode stay usable.
const gatewayModels = 512

// gatewayInfoLimit bounds the /v1/model/info response read with a workspace key.
const gatewayInfoLimit = 8 << 20

// Reasoning variants offered per wire API when the gateway reports reasoning support.
var gatewayLevels = map[string][]ThinkingLevel{
	APIChat:      {{Value: "low", Label: "Low"}, {Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}},
	APIResponses: {{Value: "low", Label: "Low"}, {Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}},
	APIMessages:  {{Value: "high", Label: "High"}, {Value: "max", Label: "Max"}},
}

type gatewayCatalog struct {
	service *Service
	ttl     time.Duration
	model   string
	mu      sync.Mutex
	cached  map[Scope]gatewayEntry
}

type gatewayEntry struct {
	catalog Catalog
	at      time.Time
}

func (s *Service) configureGatewayCatalog(c GatewayCatalog) error {
	ttl := 5 * time.Minute
	if c.TTL != "" {
		var err error
		if ttl, err = time.ParseDuration(c.TTL); err != nil || ttl < 10*time.Second || ttl > 24*time.Hour {
			return ErrDenied
		}
	}
	if c.DefaultModel != "" && !validModel(c.DefaultModel) {
		return ErrDenied
	}
	s.gateway = &gatewayCatalog{service: s, ttl: ttl, model: c.DefaultModel, cached: map[Scope]gatewayEntry{}}
	return nil
}

// catalog returns the workspace key's models; a failed refresh reuses the last good catalog.
func (g *gatewayCatalog) catalog(ctx context.Context, scope Scope) (Catalog, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.cached[scope]
	if ok && time.Since(entry.at) < g.ttl {
		return cloneCatalog(entry.catalog), nil
	}
	fresh, err := g.fetch(ctx, scope)
	if err != nil {
		if ok {
			return cloneCatalog(entry.catalog), nil
		}
		return Catalog{}, ErrDenied
	}
	g.cached[scope] = gatewayEntry{fresh, time.Now()}
	return cloneCatalog(fresh), nil
}

type modelInfo struct {
	Data []struct {
		Name   string `json:"model_name"`
		Params struct {
			Model string `json:"model"`
		} `json:"litellm_params"`
		Info struct {
			Mode      string   `json:"mode"`
			Provider  string   `json:"litellm_provider"`
			Input     int      `json:"max_input_tokens"`
			Output    int      `json:"max_output_tokens"`
			Reasoning bool     `json:"supports_reasoning"`
			InputUSD  *float64 `json:"input_cost_per_token"`
			OutputUSD *float64 `json:"output_cost_per_token"`
			CacheUSD  *float64 `json:"cache_read_input_token_cost"`
		} `json:"model_info"`
	} `json:"data"`
}

// fetch reads /v1/model/info with the workspace key. Only listed fields are kept:
// deployment parameters such as upstream bases never leave this function.
func (g *gatewayCatalog) fetch(ctx context.Context, scope Scope) (Catalog, error) {
	target, err := g.service.Acquire(ctx, scope)
	if err != nil {
		return Catalog{}, ErrDenied
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target.Gateway+"/v1/model/info", nil)
	if err != nil {
		return Catalog{}, ErrDenied
	}
	req.Header.Set("Authorization", "Bearer "+target.Key.Reveal())
	resp, err := g.service.client.Do(req)
	if err != nil {
		return Catalog{}, ErrDenied
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, gatewayInfoLimit+1))
	var info modelInfo
	if err != nil || resp.StatusCode != 200 || len(data) > gatewayInfoLimit || json.Unmarshal(data, &info) != nil {
		return Catalog{}, ErrDenied
	}
	c := gatewayModelsCatalog(info, g.model)
	if !validCatalogSize(c, gatewayModels) {
		return Catalog{}, ErrDenied
	}
	return c, nil
}

func gatewayModelsCatalog(info modelInfo, defaultModel string) Catalog {
	models := map[string]Model{}
	names := []string{}
	for _, d := range info.Data {
		api, ok := wireAPI(d.Info.Provider, d.Params.Model, d.Info.Mode)
		if _, seen := models[d.Name]; seen || !ok || !validModel(d.Name) {
			continue
		}
		m := Model{API: api, Context: d.Info.Input, Output: d.Info.Output}
		if m.Context < 1 || m.Context > 10000000 {
			m.Context = 128000
		}
		if m.Output < 1 || m.Output > m.Context {
			m.Output = min(16384, m.Context)
		}
		if d.Info.Reasoning {
			m.Thinking = &Thinking{SupportedLevels: append([]ThinkingLevel(nil), gatewayLevels[api]...)}
		}
		if in, out := perMillion(d.Info.InputUSD), perMillion(d.Info.OutputUSD); in > 0 || out > 0 {
			m.Cost = &Cost{Input: in, Output: out, CacheRead: perMillion(d.Info.CacheUSD)}
		}
		if api == APIChat {
			m.API = ""
		}
		models[d.Name] = m
		names = append(names, d.Name)
	}
	if len(names) > gatewayModels {
		sort.Strings(names)
		for _, name := range names[gatewayModels:] {
			delete(models, name)
		}
	}
	c := Catalog{Models: models}
	if _, ok := models[defaultModel]; ok {
		c.DefaultModel = defaultModel
	}
	return c
}

// wireAPI maps a LiteLLM provider to the native API that keeps prompt caching:
// Anthropic models use Messages, OpenAI models Responses, others chat completions.
// Non-chat models (embeddings, images, audio) are not agent models.
func wireAPI(provider, model, mode string) (string, bool) {
	if mode != "" && mode != "chat" && mode != "responses" {
		return "", false
	}
	if provider == "" {
		provider, _, _ = strings.Cut(model, "/")
	}
	switch provider {
	case "anthropic":
		return APIMessages, true
	case "openai":
		return APIResponses, true
	}
	return APIChat, true
}

func perMillion(perToken *float64) float64 {
	if perToken == nil || math.IsNaN(*perToken) || *perToken < 0 || *perToken > 1 {
		return 0
	}
	return math.Round(*perToken*1e6*10000) / 10000
}
