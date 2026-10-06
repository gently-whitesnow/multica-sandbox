package opencode

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const inferenceProvider = "managed-inference"

type Inference interface {
	Acquire(context.Context, inference.Scope) (inference.Target, error)
	Catalog(context.Context, inference.Scope) (inference.Catalog, error)
}

// grantInference binds the workspace key to a new opaque credential; the key stays in the controller.
func (r *running) grantInference(ctx context.Context) error {
	if r.adapter.Inference == nil {
		return nil
	}
	target, err := r.adapter.Inference.Acquire(ctx, inference.Scope{Server: r.adapter.Server, WorkspaceID: r.task.WorkspaceID})
	if err != nil {
		return fmt.Errorf("inference binding: %w", ErrDenied)
	}
	upstream := relay.Upstream{Origin: target.Gateway, Credential: target.Key.Reveal(), Header: inference.Attribution(r.task.WorkspaceID, r.task.AgentID, r.task.ID)}
	if r.inferenceToken, err = r.adapter.InferenceRelay.Issue(r.task.AttemptKey(), upstream); err != nil {
		return fmt.Errorf("inference relay grant: %w", ErrDenied)
	}
	return nil
}
func (r *running) configureInference(ctx context.Context, config []byte) ([]byte, error) {
	if r.adapter.Inference == nil {
		return config, nil
	}
	var projected map[string]any
	if json.Unmarshal(config, &projected) != nil {
		return nil, ErrDenied
	}
	selection, err := r.selectInference(ctx)
	if err != nil {
		return nil, err
	}
	r.model = inferenceProvider + "/" + selection.model
	models := nativeModels(selection.catalog)
	models[selection.model] = nativeModel(selection.model, selection.metadata, selection.effort)
	projected["model"] = inferenceProvider + "/" + selection.model
	if selection.effort != "" {
		projected["agent"] = map[string]any{"build": map[string]string{"variant": selection.effort, "model": inferenceProvider + "/" + selection.model}}
	}
	projected["enabled_providers"] = []string{inferenceProvider}
	// The opaque attempt credential is the provider key; the relay replaces it with the workspace key.
	options := map[string]string{"baseURL": r.adapter.InferenceRelayURL + "/v1", "apiKey": r.inferenceToken}
	projected["provider"] = map[string]any{inferenceProvider: map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Managed inference", "options": options, "models": models}}
	return json.Marshal(projected)
}
