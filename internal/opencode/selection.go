package opencode

import (
	"context"
	"fmt"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

type selection struct {
	model, effort string
	// api is the wire API the selected model is reached through.
	api      string
	metadata inference.Model
	catalog  inference.Catalog
}

func safeSelection(s string, limit int) bool {
	if len(s) == 0 || len(s) > limit {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func (r *running) selectInference(ctx context.Context) (selection, error) {
	// The runtime catalog is the one Multica's picker showed; per-agent access stays with the gateway.
	catalog, err := r.adapter.Inference.Catalog(ctx, inference.Scope{Server: r.adapter.Server, WorkspaceID: r.task.WorkspaceID})
	if ctx.Err() != nil {
		return selection{}, ctx.Err()
	}
	model := r.task.Agent.Model
	if model == "" {
		if err != nil || catalog.DefaultModel == "" {
			return selection{}, fmt.Errorf("inference default model unavailable: %w", ErrDenied)
		}
		model = catalog.DefaultModel
	}
	api, model := inference.SplitModel(model)
	if !safeSelection(model, 256) {
		return selection{}, fmt.Errorf("invalid inference model: %w", ErrDenied)
	}
	metadata, known := catalog.Models[model]
	// An explicit selection may be newer than discovery. This is preparation metadata, not a quota.
	if !known {
		metadata = inference.Model{}
	}
	// A provider prefix selects the API, so legacy managed-inference/ selections stay on chat;
	// a bare name follows the catalog.
	if api == "" {
		api = metadata.WireAPI()
	}
	effort := r.task.Agent.ThinkingLevel
	if effort == "" && metadata.Thinking != nil {
		effort = metadata.Thinking.DefaultLevel
	}
	if effort != "" && !inference.ValidEffort(effort) {
		return selection{}, fmt.Errorf("invalid inference reasoning option: %w", ErrDenied)
	}
	// Explicit safe efforts are forwarded even when absent from an advisory catalog.
	if effort != "" && api == inference.APIMessages && thinkingBudget[effort] == 0 {
		return selection{}, fmt.Errorf("unsupported Messages reasoning option: %w", ErrDenied)
	}
	return selection{model: model, effort: effort, api: api, metadata: metadata, catalog: catalog}, nil
}

// thinkingBudget maps Messages reasoning variants to extended-thinking token budgets.
var thinkingBudget = map[string]int{"low": 4096, "medium": 8192, "high": 16000, "max": 31999}

// nativeModel projects the selected model; its variants carry the provider's reasoning option.
func nativeModel(name, api string, m inference.Model, effort string) map[string]any {
	variants := map[string]any{}
	levels := []string{}
	if m.Thinking != nil {
		for _, level := range m.Thinking.SupportedLevels {
			levels = append(levels, level.Value)
		}
	}
	if effort != "" {
		levels = append(levels, effort)
	}
	for _, level := range levels {
		if api != inference.APIMessages {
			variants[level] = map[string]string{"reasoningEffort": level}
		} else if budget := thinkingBudget[level]; budget > 0 {
			variants[level] = map[string]any{"thinking": map[string]any{"type": "enabled", "budgetTokens": budget}}
		}
	}
	label := m.Label
	if label == "" {
		label = name
	}
	native := map[string]any{"name": label, "limit": map[string]int{"context": m.Context, "output": m.Output}, "variants": variants}
	if m.Thinking != nil {
		native["reasoning"] = true
	}
	if m.Cost != nil {
		native["cost"] = map[string]float64{"input": m.Cost.Input, "output": m.Cost.Output, "cache_read": m.Cost.CacheRead}
	}
	return native
}
