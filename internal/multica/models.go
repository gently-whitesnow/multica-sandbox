package multica

import (
	"context"
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

// HeartbeatModels answers upstream's runtime-scoped pending model-list request
// with the runtime's advisory catalog, like the upstream daemon's result report.
func (c *Client) HeartbeatModels(ctx context.Context, runtime string, discover func(context.Context) ([]ModelEntry, error)) error {
	if !validID(runtime) {
		return fmt.Errorf("invalid runtime ID")
	}
	var out struct {
		Pending *struct {
			ID string `json:"id"`
		} `json:"pending_model_list"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/daemon/heartbeat", map[string]string{"runtime_id": runtime}, &out); err != nil {
		return err
	}
	if out.Pending == nil {
		return nil
	}
	if out.Pending.ID == "" {
		return fmt.Errorf("invalid model discovery request ID")
	}
	report := map[string]any{"status": "failed", "error": "Inference model catalog unavailable"}
	if models, err := discover(ctx); err == nil && len(models) > 0 {
		report = map[string]any{"status": "completed", "supported": true, "models": models}
	}
	return c.call(ctx, http.MethodPost, "/api/daemon/runtimes/"+runtime+"/models/"+url.PathEscape(out.Pending.ID)+"/result", report, nil)
}
