package main

import (
	"context"
	"fmt"
)

func check(ctx context.Context) error {
	c, err := load()
	if err != nil {
		return err
	}
	inference, err := identity(ctx, c, "inference")
	if err != nil {
		return err
	}
	mcpToken, err := identity(ctx, c, "mcp")
	if err != nil {
		return err
	}
	checks := []struct {
		name, address, token string
		body                 any
	}{
		{"unauthenticated inference denied", "http://litellm:4000/v1/chat/completions", "", map[string]any{"model": "demo", "messages": []any{}}},
		{"MCP audience cannot authorize inference", "http://litellm:4000/v1/chat/completions", mcpToken, map[string]any{"model": "demo", "messages": []any{}}},
		{"inference identity cannot authorize MCP", "http://mcp:8080/mcp", inference, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}},
		{"inactive task cannot authorize MCP", "http://mcp:8080/mcp", mcpToken, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}},
	}
	for _, item := range checks {
		status, err := exchange(ctx, "POST", item.address, item.token, item.body, nil)
		if err != nil || !(status == 401 || status == 403) {
			return fmt.Errorf("%s: expected 401/403, got %d", item.name, status)
		}
		fmt.Println("PASS", item.name)
	}
	return nil
}

func denied(ctx context.Context, name, address, token string, body any) error {
	status, err := exchange(ctx, "POST", address, token, body, nil)
	if err != nil || !(status == 401 || status == 403) {
		return fmt.Errorf("%s: expected 401/403, got %d", name, status)
	}
	fmt.Println("PASS", name)
	return nil
}
func checkActive(ctx context.Context, token, mcpToken string) error {
	message := []map[string]string{{"role": "user", "content": "fixture check"}}
	for _, item := range []struct {
		name, path string
		body       map[string]any
	}{
		{"ungranted model denied", "/v1/chat/completions", map[string]any{"model": "forbidden", "messages": message}},
		{"provider endpoint override denied", "/v1/chat/completions", map[string]any{"model": "demo", "messages": message, "api_base": "http://forbidden.invalid/v1"}},
		{"provider credential override denied", "/v1/chat/completions", map[string]any{"model": "demo", "messages": message, "api_key": "forbidden"}},
		{"unsupported inference route denied", "/v1/responses", map[string]any{"model": "demo", "input": "fixture check"}},
	} {
		if err := denied(ctx, item.name, "http://litellm:4000"+item.path, token, item.body); err != nil {
			return err
		}
	}
	var rejected struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	status, err := exchange(ctx, "POST", "http://mcp:8080/mcp", mcpToken, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "read_task", "arguments": map[string]string{"issue": "40000000-0000-4000-8000-000000000099"}}}, &rejected)
	if err != nil || status != 200 || !rejected.Result.IsError {
		return fmt.Errorf("ungranted MCP resource was not denied")
	}
	fmt.Println("PASS ungranted MCP resource denied")
	return nil
}
func checkEnded(ctx context.Context, inference, mcpToken string) error {
	if err := denied(ctx, "ended task inference denied with its original token", "http://litellm:4000/v1/chat/completions", inference, map[string]any{"model": "demo", "messages": []map[string]string{{"role": "user", "content": "fixture check"}}, "max_tokens": 1}); err != nil {
		return err
	}
	return denied(ctx, "ended task MCP denied with its original token", "http://mcp:8080/mcp", mcpToken, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
}
