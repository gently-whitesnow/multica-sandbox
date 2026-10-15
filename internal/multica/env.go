package multica

import (
	"context"
	"fmt"
	"net/http"
)

// WorkspaceEnv reads a workspace's global env (upstream PR 4521): owner/admin only, and
// every read is audited as workspace_env_revealed. A server without workspace env answers
// 404, reported as ok false.
func (c *Client) WorkspaceEnv(ctx context.Context, workspace string) (env SecretEnv, ok bool, err error) {
	if !validID(workspace) {
		return SecretEnv{}, false, fmt.Errorf("invalid workspace ID")
	}
	var out struct {
		WorkspaceID string    `json:"workspace_id"`
		GlobalEnv   SecretEnv `json:"global_env"`
	}
	if err = c.call(ctx, http.MethodGet, "/api/workspaces/"+workspace+"/env", nil, &out); err != nil {
		if Missing(err) {
			return SecretEnv{}, false, nil
		}
		return SecretEnv{}, false, err
	}
	if out.WorkspaceID != workspace {
		return SecretEnv{}, false, fmt.Errorf("workspace env for another workspace")
	}
	return out.GlobalEnv, true, nil
}
