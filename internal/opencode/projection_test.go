package opencode

import (
	"encoding/json"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"strings"
	"testing"
)

func safeTask() multica.Task {
	return multica.Task{ID: "40000000-0000-4000-8000-000000000001", RuntimeID: "50000000-0000-4000-8000-000000000001", DispatchedAt: "2026-10-05T12:00:00Z", StartClaimSupported: true, WorkspaceID: "10000000-0000-4000-8000-000000000001", AgentID: "20000000-0000-4000-8000-000000000001", Agent: &multica.Agent{ID: "20000000-0000-4000-8000-000000000001", MCPConfig: json.RawMessage(`{"mcpServers":{"selected":{"url":"https://tools.example.invalid/mcp"}}}`)}}
}
func TestProjectionDeniesClaimCredentials(t *testing.T) {
	for _, config := range []string{
		`{"mcpServers":{"x":{"url":"https://tools/mcp","headers":{"aUtHoRiZaTiOn":"Bearer private"}}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp","headers":{"x-api-key":"private"}}}}`,
		`{"mcpServers":{"x":{"command":"/bin/sh","args":["-c","evil"]}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp","oauth":{"clientSecret":"private"}}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp","env":{"SECRET":"private"}}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp"}}} {}`,
	} {
		task := safeTask()
		task.Agent.MCPConfig = json.RawMessage(config)
		if _, err := Select(task); err == nil {
			t.Fatal("unsafe connection accepted")
		}
	}
	task := safeTask()
	task.Agent.ID = "20000000-0000-4000-8000-000000000002"
	if _, err := Select(task); err == nil {
		t.Fatal("spoofed agent accepted")
	}
}
func TestNativeStoreContract(t *testing.T) {
	selected, err := Select(safeTask())
	if err != nil {
		t.Fatal(err)
	}
	config, err := Config(selected)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "oauth\":false") || strings.Contains(string(config), "headers") {
		t.Fatal("native OAuth disabled")
	}
	store, _ := json.Marshal(map[string]Entry{"selected": {ServerURL: selected["selected"].URL, Tokens: Token{"access", 123}}})
	if string(store) != `{"selected":{"serverUrl":"https://tools.example.invalid/mcp","tokens":{"accessToken":"access","expiresAt":123}}}` {
		t.Fatal("native store contract changed")
	}
}

func TestInferenceTaskCanHaveNoMCP(t *testing.T) {
	task := safeTask()
	task.Agent.MCPConfig = json.RawMessage(`{}`)
	selected, err := Select(task)
	if err != nil || len(selected) != 0 {
		t.Fatal("inference task required an MCP connection", err)
	}
}
