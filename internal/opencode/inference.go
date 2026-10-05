package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

const InferencePath = "/workspace/inference-token.json"
const InferencePluginPath = "/workspace/inference-auth.mjs"
const ProviderAuthPath = "/workspace/data/opencode/auth.json"
const inferenceProvider = "managed-inference"

type Inference interface {
	Acquire(context.Context, identity.Ref) (inference.Session, error)
	Catalog(context.Context, identity.Ref) (inference.Catalog, error)
}

func (r *running) refreshInference(ctx context.Context, ref identity.Ref) (bool, error) {
	if r.adapter.Inference == nil {
		return false, nil
	}
	session := r.inference
	changed := false
	if time.Until(session.Token.ExpiresAt) <= 10*time.Second {
		next, err := r.adapter.Inference.Acquire(ctx, ref)
		if err != nil || time.Until(next.Token.ExpiresAt) <= 10*time.Second {
			return false, fmt.Errorf("inference issuance: %w", ErrDenied)
		}
		// Delivery recipients are fixed; catalog changes do not affect identity renewal.
		if session.Target.URL != "" && session.Target != next.Target {
			return false, fmt.Errorf("inference binding changed: %w", ErrDenied)
		}
		for _, remote := range r.connections {
			if remote.URL == next.URL {
				return false, fmt.Errorf("inference/MCP recipient conflict: %w", ErrDenied)
			}
		}
		session = next
		changed = true
	}
	grant := r.grant("renew")
	grant.InferenceURL = session.URL
	grant.TokenHash = attempt.Fingerprint(session.Token.Bearer())
	grant.ExpiresAt = min(session.Token.ExpiresAt.Unix(), time.Now().Add(15*time.Second).Unix())
	if err := r.adapter.Authority.Apply(ctx, grant); err != nil {
		return false, err
	}
	r.inference = session
	return changed, nil
}
func (r *running) projectInference(ctx context.Context) error {
	if r.adapter.Inference == nil {
		return nil
	}
	if time.Until(r.inference.Token.ExpiresAt) <= time.Second {
		return ErrDenied
	}
	data, err := json.Marshal(struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
	}{r.inference.Token.Bearer(), r.inference.Token.ExpiresAt.Unix()})
	if err != nil {
		return ErrDenied
	}
	return r.workload.Write(ctx, InferencePath, data)
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
	models := nativeModels(selection.catalog)
	models[selection.model] = nativeModel(selection.model, selection.metadata, selection.effort)
	projected["model"] = inferenceProvider + "/" + selection.model
	if selection.effort != "" {
		projected["agent"] = map[string]any{"build": map[string]string{"variant": selection.effort, "model": inferenceProvider + "/" + selection.model}}
	}
	projected["enabled_providers"] = []string{inferenceProvider}
	projected["provider"] = map[string]any{inferenceProvider: map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Managed inference", "options": map[string]any{"baseURL": r.inference.URL}, "models": models}}
	projected["plugin"] = []string{"file://" + InferencePluginPath}
	plugin, err := inferencePlugin(r.inference.URL)
	if err != nil {
		return nil, err
	}
	if err = r.workload.Write(ctx, InferencePluginPath, plugin); err != nil {
		return nil, err
	}
	// This marker enables OpenCode's auth loader; it is never used as authorization.
	if err = r.workload.Write(ctx, ProviderAuthPath, []byte(`{"managed-inference":{"type":"api","key":"managed-token"}}`)); err != nil {
		return nil, err
	}
	return json.Marshal(projected)
}
func inferencePlugin(endpoint string) ([]byte, error) {
	recipient, err := json.Marshal(endpoint + "/chat/completions")
	if err != nil {
		return nil, ErrDenied
	}
	return []byte(`const denied = () => new Response(JSON.stringify({error: {message: "Inference identity denied", type: "authentication_error"}}), {status: 401, headers: {"Content-Type": "application/json"}});
export const ManagedInference = async () => ({
 auth: {
  provider: "managed-inference", methods: [],
  loader: async () => ({
   apiKey: "managed-token",
   fetch: async (input, init) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url !== ` + string(recipient) + `) return denied();
    let token;
    try { token = await Bun.file("/workspace/inference-token.json").json(); }
    catch { return denied(); }
    if (typeof token.access_token !== "string" || !token.access_token || /[\r\n]/.test(token.access_token) || !Number.isSafeInteger(token.expires_at) || token.expires_at <= Date.now() / 1000) return denied();
    const headers = new Headers(input instanceof Request ? input.headers : undefined);
    new Headers(init?.headers).forEach((value, key) => headers.set(key, value));
    headers.set("Authorization", "Bearer " + token.access_token);
    return fetch(input, {...init, headers, redirect: "error"});
   }
  })
 }
});`), nil
}
