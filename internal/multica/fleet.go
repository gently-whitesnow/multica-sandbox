package multica

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Workspace struct {
	ID string `json:"id"`
}

func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	var out []Workspace
	if err := c.call(ctx, http.MethodGet, "/api/daemon/workspaces", nil, &out); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, w := range out {
		if !validID(w.ID) || seen[w.ID] {
			return nil, fmt.Errorf("invalid workspace discovery")
		}
		seen[w.ID] = true
	}
	return out, nil
}

// CoAuthoredBy mirrors upstream workspaceCoAuthoredByEnabled: the Co-authored-by hook
// needs github_enabled and co_authored_by_enabled, both true when absent. Callers keep
// their last value on error, as upstream keeps cached settings when a refresh fails.
func (c *Client) CoAuthoredBy(ctx context.Context, workspace string) (bool, error) {
	var out struct {
		Settings struct {
			GitHub   *bool `json:"github_enabled"`
			CoAuthor *bool `json:"co_authored_by_enabled"`
		} `json:"settings"`
	}
	if !validID(workspace) {
		return true, fmt.Errorf("invalid workspace")
	}
	if err := c.call(ctx, http.MethodGet, "/api/daemon/workspaces/"+workspace+"/repos", nil, &out); err != nil {
		return true, err
	}
	return (out.Settings.GitHub == nil || *out.Settings.GitHub) && (out.Settings.CoAuthor == nil || *out.Settings.CoAuthor), nil
}

func (c *Client) ClaimBatch(ctx context.Context, daemon string, scopes map[string]string, limit int) ([]Task, error) {
	if !validID(daemon) || limit < 1 || limit > 32 {
		return nil, fmt.Errorf("invalid batch capacity or daemon")
	}
	ids := make([]string, 0, len(scopes))
	for id, workspace := range scopes {
		if !validID(id) || !validID(workspace) {
			return nil, fmt.Errorf("invalid runtime scope")
		}
		ids = append(ids, id)
	}
	var out struct {
		Tasks []Task `json:"tasks"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"daemon_id": daemon, "runtime_ids": ids, "max_tasks": limit}, &out); err != nil {
		return nil, err
	}
	if len(out.Tasks) > limit {
		return nil, fmt.Errorf("claim exceeds reserved capacity")
	}
	seen := map[string]bool{}
	for _, t := range out.Tasks {
		if !validID(t.ID) || t.WorkspaceID == "" || scopes[t.RuntimeID] != t.WorkspaceID || !t.StartClaimSupported || seen[t.ID] {
			return nil, fmt.Errorf("invalid batch claim scope")
		}
		if _, err := time.Parse(time.RFC3339Nano, t.DispatchedAt); err != nil {
			return nil, fmt.Errorf("invalid dispatch fence")
		}
		seen[t.ID] = true
	}
	return out.Tasks, nil
}
