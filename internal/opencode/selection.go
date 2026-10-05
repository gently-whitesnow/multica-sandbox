package opencode

import (
	"context"
	"fmt"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

type selection struct {
	model, effort string
	metadata      inference.Model
	catalog       inference.Catalog
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
	ref := identity.Ref{Server: r.adapter.Server, WorkspaceID: r.task.WorkspaceID, AgentID: r.task.AgentID}
	catalog, err := r.adapter.Inference.Catalog(ctx, ref)
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
	model = strings.TrimPrefix(model, inferenceProvider+"/")
	if !safeSelection(model, 256) {
		return selection{}, fmt.Errorf("invalid inference model: %w", ErrDenied)
	}
	metadata, known := catalog.Models[model]
	// An explicit selection may be newer than discovery. This is preparation metadata, not a quota.
	if !known {
		metadata = inference.Model{}
	}
	effort := r.task.Agent.ThinkingLevel
	if effort == "" && metadata.Thinking != nil {
		effort = metadata.Thinking.DefaultLevel
	}
	if effort != "" && !inference.ValidEffort(effort) {
		return selection{}, fmt.Errorf("invalid inference reasoning option: %w", ErrDenied)
	}
	// Explicit safe efforts are forwarded even when absent from an advisory catalog.
	return selection{model: model, effort: effort, metadata: metadata, catalog: catalog}, nil
}
func nativeModels(c inference.Catalog) map[string]any {
	models := map[string]any{}
	for name, m := range c.Models {
		models[name] = nativeModel(name, m, "")
	}
	return models
}
func nativeModel(name string, m inference.Model, effort string) map[string]any {
	variants := map[string]any{}
	if m.Thinking != nil {
		for _, level := range m.Thinking.SupportedLevels {
			variants[level.Value] = map[string]string{"reasoningEffort": level.Value}
		}
	}
	if effort != "" {
		variants[effort] = map[string]string{"reasoningEffort": effort}
	}
	label := m.Label
	if label == "" {
		label = name
	}
	return map[string]any{"name": label, "limit": map[string]int{"context": m.Context, "output": m.Output}, "variants": variants}
}
