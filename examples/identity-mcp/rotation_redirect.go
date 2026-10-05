package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
	"os"
	"time"
)

func rotationRedirect(ctx context.Context, base *opencode.Adapter, states *rotationStatus, task multica.Task) error {
	config := resolverConfig()
	config.MCP = append(config.MCP, identity.MCPRule{URL: "http://gateway:8080/redirect", Issuer: "fixture"})
	service, err := identity.New(config)
	if err != nil {
		return err
	}
	adapter := *base
	adapter.Issuer = service
	adapter.Command = []string{"/usr/local/bin/opencode", "mcp", "list"}
	task.Agent = &multica.Agent{ID: task.AgentID, MCPConfig: json.RawMessage(`{"mcpServers":{"fixture":{"url":"http://gateway:8080/redirect"}}}`)}
	task.ID = "40000000-0000-4000-8000-000000000020"
	task.DispatchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	states.set(task.ID, "running")
	run, err := adapter.Start(ctx, task)
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_ = run.Wait(waitCtx)
	if err := run.Remove(context.Background()); err != nil {
		return err
	}
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	status, data, err := doRequest(ctx, "GET", "http://gateway:8080/evidence", nil, string(secret))
	var evidence map[string][2]int
	if err != nil || status != 200 || json.Unmarshal(data, &evidence) != nil {
		return fmt.Errorf("redirect evidence unavailable")
	}
	if stats := evidence["redirects"]; stats[0] == 0 || stats[1] != 0 {
		return fmt.Errorf("native redirect contract failed: requests=%d bearer-leaks=%d", stats[0], stats[1])
	}
	fmt.Println("PASS native MCP requests refuse redirects without forwarding bearer identity")
	return nil
}
