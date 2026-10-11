package opencode

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const inferenceProvider = inference.ChatProvider

// providerPackages are the AI SDK packages OpenCode bundles for each wire API.
var providerPackages = map[string][2]string{
	inference.APIChat:      {"@ai-sdk/openai-compatible", "Managed inference"},
	inference.APIResponses: {"@ai-sdk/openai", "Managed inference (Responses)"},
	inference.APIMessages:  {"@ai-sdk/anthropic", "Managed inference (Messages)"},
}

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
	// Only the selected model is projected: the catalog may hold hundreds of advisory entries.
	provider, pkg := inference.Provider(selection.api), providerPackages[selection.api]
	r.model = provider + "/" + selection.model
	models := map[string]any{selection.model: nativeModel(selection.model, selection.api, selection.metadata, selection.effort)}
	projected["model"] = r.model
	if selection.effort != "" {
		projected["agent"] = map[string]any{"build": map[string]string{"variant": selection.effort, "model": r.model}}
	}
	projected["enabled_providers"] = []string{provider}
	// The opaque attempt credential is the provider key (a bearer token, or X-Api-Key for
	// Messages); the relay replaces it with the workspace key.
	options := map[string]string{"baseURL": r.adapter.InferenceRelayURL + "/v1", "apiKey": r.inferenceToken}
	projected["provider"] = map[string]any{provider: map[string]any{"npm": pkg[0], "name": pkg[1], "options": options, "models": models}}
	return json.Marshal(projected)
}
