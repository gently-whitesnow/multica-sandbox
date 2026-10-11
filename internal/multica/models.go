package multica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ModelEntry mirrors upstream's runtime model-list wire shape.
type ModelEntry struct {
	ID       string         `json:"id"`
	Label    string         `json:"label"`
	Provider string         `json:"provider,omitempty"`
	Default  bool           `json:"default,omitempty"`
	Thinking *ModelThinking `json:"thinking,omitempty"`
}
type ModelThinking struct {
	SupportedLevels []ThinkingLevel `json:"supported_levels"`
	DefaultLevel    string          `json:"default_level,omitempty"`
}
type ThinkingLevel struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Heartbeat sends a liveness heartbeat and answers the runtime-scoped requests upstream
// queues on it, like the upstream daemon: the model list from discover (nil: none), and
// runtime updates and local skills as unsupported, so the UI never waits for a timeout.
func (c *Client) Heartbeat(ctx context.Context, runtime string) error {
	return c.HeartbeatModels(ctx, runtime, nil)
}

type pendingRequest struct {
	ID string `json:"id"`
}

const unsupported = "not supported by sandbox runtime"

// HeartbeatModels is Heartbeat with an advisory model catalog for pending model lists.
func (c *Client) HeartbeatModels(ctx context.Context, runtime string, discover func(context.Context) ([]ModelEntry, error)) error {
	if !validID(runtime) {
		return fmt.Errorf("invalid runtime ID")
	}
	var out struct {
		Models  *pendingRequest  `json:"pending_model_list"`
		Update  *pendingRequest  `json:"pending_update"`
		Skills  *pendingRequest  `json:"pending_local_skills"`
		Import  *pendingRequest  `json:"pending_local_skill_import"`
		Imports []pendingRequest `json:"pending_local_skill_imports"`
	}
	data, err := c.send(ctx, http.MethodPost, "/api/daemon/heartbeat", map[string]string{"runtime_id": runtime}, true)
	if err != nil {
		return err
	}
	// An empty acknowledgement queues nothing.
	if len(bytes.TrimSpace(data)) > 0 && json.Unmarshal(data, &out) != nil {
		return fmt.Errorf("invalid Multica response")
	}
	type result struct {
		path string
		id   string
		body map[string]any
	}
	var reports []result
	if out.Models != nil {
		report := map[string]any{"status": "failed", "error": "Inference model catalog unavailable"}
		if discover != nil {
			if models, err := discover(ctx); err == nil && len(models) > 0 {
				report = map[string]any{"status": "completed", "supported": true, "models": models}
			}
		}
		reports = append(reports, result{"models", out.Models.ID, report})
	}
	if out.Update != nil {
		reports = append(reports, result{"update", out.Update.ID, map[string]any{"status": "failed", "error": "runtime update " + unsupported}})
	}
	if out.Skills != nil {
		// Upstream's answer for a provider without runtime-local skills or MCP servers.
		reports = append(reports, result{"local-skills", out.Skills.ID, map[string]any{"status": "completed", "skills": []any{}, "supported": false, "mcp_servers": []any{}, "mcp_supported": false}})
	}
	// The plural field repeats the singular one for daemons that know it.
	imports := map[string]bool{}
	for _, p := range append(deref(out.Import), out.Imports...) {
		if !imports[p.ID] {
			imports[p.ID] = true
			reports = append(reports, result{"local-skills/import", p.ID, map[string]any{"status": "failed", "error": "runtime local skill import " + unsupported}})
		}
	}
	for _, r := range reports {
		if r.id == "" || len(r.id) > 128 {
			err = errors.Join(err, fmt.Errorf("invalid %s request ID", r.path))
			continue
		}
		err = errors.Join(err, c.call(ctx, http.MethodPost, "/api/daemon/runtimes/"+runtime+"/"+r.path+"/"+url.PathEscape(r.id)+"/result", r.body, nil))
	}
	return err
}

func deref(p *pendingRequest) []pendingRequest {
	if p == nil {
		return nil
	}
	return []pendingRequest{*p}
}
