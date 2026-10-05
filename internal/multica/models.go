package multica

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

type ModelEntry struct {
	ID       string         `json:"id"`
	Label    string         `json:"label"`
	Provider string         `json:"provider"`
	Default  bool           `json:"default,omitempty"`
	Context  int            `json:"context"`
	Output   int            `json:"output"`
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

// HeartbeatModels reuses pending discovery, but requires an upstream agent-scoped request.
func (c *Client) HeartbeatModels(ctx context.Context, runtime string, discover func(context.Context, string) ([]ModelEntry, error)) error {
	if !validID(runtime) {
		return fmt.Errorf("invalid runtime ID")
	}
	var out struct {
		Pending *struct {
			ID      string `json:"id"`
			AgentID string `json:"agent_id"`
		} `json:"pending_model_list"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/daemon/heartbeat", map[string]string{"runtime_id": runtime}, &out); err != nil {
		return err
	}
	if out.Pending == nil {
		return nil
	}
	p := out.Pending
	if p.ID == "" {
		return fmt.Errorf("invalid model discovery request ID")
	}
	report := map[string]any{"status": "failed", "error": "Agent-scoped model discovery requires an upstream contract with agent_id and scoped catalog caching."}
	if validID(p.AgentID) {
		models, err := discover(ctx, p.AgentID)
		if err == nil {
			report = map[string]any{"status": "completed", "supported": true, "agent_id": p.AgentID, "models": models}
		} else {
			report["error"] = "Agent model catalog unavailable"
		}
	}
	return c.call(ctx, http.MethodPost, "/api/daemon/runtimes/"+runtime+"/models/"+url.PathEscape(p.ID)+"/result", report, nil)
}
