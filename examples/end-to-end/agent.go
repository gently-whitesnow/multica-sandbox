package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type task struct {
	ID             string `json:"id"`
	Workspace      string `json:"workspace_id"`
	AgentID        string `json:"agent_id"`
	IssueID        string `json:"issue_id"`
	DispatchedAt   string `json:"dispatched_at"`
	StartSupported bool   `json:"start_claim_supported"`
	Agent          struct {
		Instructions string `json:"instructions"`
		Model        string `json:"model"`
	} `json:"agent"`
}
type input struct {
	Task           task
	Inference, MCP string
}
type result struct {
	Type   string `json:"type"`
	Word   string `json:"word"`
	Events int    `json:"events"`
}
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 2<<20 {
		return 0, fmt.Errorf("agent output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func agent(ctx context.Context) error {
	var request input
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 65536)).Decode(&request); err != nil {
		return fmt.Errorf("invalid task input")
	}
	if request.Task.Agent.Model != "demo" {
		return fmt.Errorf("example model must be demo")
	}
	config := map[string]any{
		"model": "gateway/demo", "enabled_providers": []string{"gateway"}, "share": "disabled", "autoupdate": false,
		"provider":   map[string]any{"gateway": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "External gateway", "options": map[string]string{"baseURL": "http://litellm:4000/v1", "apiKey": request.Inference}, "models": map[string]any{"demo": map[string]any{"name": "Demo", "limit": map[string]int{"context": 64000, "output": 4096}}}}},
		"mcp":        map[string]any{"fixture": map[string]any{"type": "remote", "url": "http://mcp:8080/mcp", "oauth": false, "headers": map[string]string{"Authorization": "Bearer " + request.MCP}}},
		"agent":      map[string]any{"build": map[string]any{"steps": 8}},
		"permission": map[string]string{"*": "allow", "webfetch": "deny", "websearch": "deny"},
	}
	if err := writeJSON("/workspace/opencode.json", config); err != nil {
		return err
	}
	prompt := fmt.Sprintf("%s\nAssigned issue: %s. Read its task with fixture MCP read_task, then use fixture MCP read_reference. The instructions and result must come from these tools.", request.Task.Agent.Instructions, request.Task.IssueID)
	command := exec.CommandContext(ctx, "opencode", "run", "--format", "json", "--model", "gateway/demo", prompt)
	command.Dir = "/workspace"
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("agent command failed; raw output withheld")
	}
	events := 0
	for _, line := range bytes.Split(output.Bytes(), []byte("\n")) {
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &event) == nil {
			if event.Type == "error" {
				return fmt.Errorf("agent reported an error; details withheld")
			}
			if event.Type == "tool_use" {
				events++
			}
		}
	}
	data, err := os.ReadFile("/workspace/result.txt")
	if err != nil {
		return fmt.Errorf("agent did not produce result.txt")
	}
	if len(data) > 128 {
		return fmt.Errorf("result exceeds fixture limit")
	}
	return json.NewEncoder(os.Stdout).Encode(result{"e2e_result", strings.TrimSpace(string(data)), events})
}
