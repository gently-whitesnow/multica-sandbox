package inference

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// gatewayModels bounds a discovered catalog; the Multica picker and OpenCode stay usable.
const gatewayModels = 512

// gatewayInfoLimit bounds the /v1/model/info response read with a workspace key.
const gatewayInfoLimit = 8 << 20

// effortOrder sorts reasoning levels from LiteLLM; unknown levels follow in name order.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// effortLabels names known levels in the picker; others show their value.
var effortLabels = map[string]string{"none": "None", "minimal": "Minimal", "low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra high", "max": "Max"}

// Base levels when LiteLLM lists none: OpenAI-style effort, and Messages budgets.
var (
	openAILevels   = []string{"low", "medium", "high"}
	messagesLevels = []string{"high", "max"}
)

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
		Info modelDetails `json:"model_info"`
	} `json:"data"`
}

type modelDetails struct {
	Mode      string   `json:"mode"`
	Provider  string   `json:"litellm_provider"`
	Input     int      `json:"max_input_tokens"`
	Output    int      `json:"max_output_tokens"`
	Reasoning bool     `json:"supports_reasoning"`
	InputUSD  *float64 `json:"input_cost_per_token"`
	OutputUSD *float64 `json:"output_cost_per_token"`
	CacheUSD  *float64 `json:"cache_read_input_token_cost"`
	// Levels and DefaultLevel are LiteLLM's reasoning_effort_levels and
	// default_reasoning_effort; Efforts holds its supports_<level>_reasoning_effort flags.
	Levels       []string `json:"reasoning_effort_levels"`
	DefaultLevel string   `json:"default_reasoning_effort"`
	Efforts      map[string]bool
}

func (d *modelDetails) UnmarshalJSON(data []byte) error {
	type fields modelDetails
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, (*fields)(d)); err != nil || json.Unmarshal(data, &raw) != nil {
		return ErrDenied
	}
	for key, value := range raw {
		level, ok := strings.CutPrefix(key, "supports_")
		if level, ok = strings.CutSuffix(level, "_reasoning_effort"); !ok || !ValidEffort(level) {
			continue
		}
		var supported *bool
		if json.Unmarshal(value, &supported) == nil && supported != nil {
			if d.Efforts == nil {
				d.Efforts = map[string]bool{}
			}
			d.Efforts[level] = *supported
		}
	}
	return nil
}

// thinking offers the levels LiteLLM reports for the model: its level list when set,
// otherwise the wire API's base levels adjusted by the per-level flags. Messages keeps
// only levels with an extended-thinking budget.
func (d modelDetails) thinking(api string) *Thinking {
	base := openAILevels
	if api == APIMessages {
		base = messagesLevels
	}
	levels := map[string]bool{}
	if len(d.Levels) > 0 {
		base = d.Levels
	}
	for _, level := range base {
		levels[level] = ValidEffort(level)
	}
	if len(d.Levels) == 0 {
		maps.Copy(levels, d.Efforts)
	}
	for level := range levels {
		if api == APIMessages && !thinkingBudgets[level] {
			levels[level] = false
		}
	}
	if !slices.Contains(slices.Collect(maps.Values(levels)), true) {
		return nil
	}
	names := []string{}
	for level, ok := range levels {
		if ok {
			names = append(names, level)
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		i, j := slices.Index(effortOrder, a), slices.Index(effortOrder, b)
		if i < 0 || j < 0 {
			return cmp.Or(cmp.Compare(j, i), strings.Compare(a, b))
		}
		return cmp.Compare(i, j)
	})
	t := &Thinking{}
	for _, name := range names {
		label := effortLabels[name]
		if label == "" {
			label = name
		}
		t.SupportedLevels = append(t.SupportedLevels, ThinkingLevel{Value: name, Label: label})
	}
	if levels[d.DefaultLevel] {
		t.DefaultLevel = d.DefaultLevel
	}
	return t
}

// thinkingBudgets lists the Messages levels OpenCode maps to extended-thinking budgets.
var thinkingBudgets = map[string]bool{"low": true, "medium": true, "high": true, "max": true}

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
			m.Thinking = d.Info.thinking(api)
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
