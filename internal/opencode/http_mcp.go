package opencode

import (
	"context"
	"net/url"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

// httpMCP requires discovery of every selected server before the first turn.
// Native connected status includes a successful initial tools/list request.
func (r *running) httpMCP(ctx context.Context) error {
	for len(r.connections) != 0 {
		var response struct {
			Data []struct {
				Name   string
				Status struct{ Status string }
			}
		}
		if err := r.httpAPI(ctx, "GET", "/api/mcp?directory="+url.QueryEscape(repo.WorkDir), nil, &response); err != nil {
			return err
		}
		connected := map[string]bool{}
		for _, server := range response.Data {
			if _, selected := r.connections[server.Name]; !selected {
				continue
			}
			switch server.Status.Status {
			case "connected":
				connected[server.Name] = true
			case "failed", "needs_auth", "disabled":
				return &execution.AgentFailure{Message: "OpenCode selected HTTP MCP server is " + server.Status.Status}
			}
		}
		if len(connected) == len(r.connections) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(httpPollInterval):
		}
	}
	return nil
}
