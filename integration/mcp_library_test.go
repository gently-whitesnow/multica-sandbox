//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

func mcpLibrary(t *testing.T, api *multica.Client, runtime multica.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base := os.Getenv("MULTICA_TEST_URL")
	request := func(method, path, body string, want int) string {
		return nativeAuthorityRequest(t, ctx, base, token(t), method, path, body, want)
	}
	taskID := enqueue(t, runtime, 700)
	agentID := "30000000-0000-4000-8000-000000000700"
	path := "/api/workspaces/" + workspace + "/mcp-servers"
	first := `{"name":"public-fixture","config":{"type":"http","url":"https://one.example.invalid/mcp?project_ref=fixture","headers":{"Authorization":"Bearer library-secret-one"}}}`
	body := request("POST", path, first, 201)
	var server struct{ ID string }
	if json.Unmarshal([]byte(body), &server) != nil || !multica.ValidID(server.ID) {
		t.Fatal("workspace MCP creation did not return ID")
	}
	if strings.Contains(body, "library-secret") || strings.Contains(body, "one.example.invalid") {
		t.Fatal("write-only MCP config leaked in library response")
	}
	binding := "/api/agents/" + agentID + "/mcp-servers"
	request("POST", binding, `{"server_id":"`+server.ID+`"}`, 200)
	for round := 0; round < 3; round++ {
		if round > 0 {
			taskID = queueMCPContinuation(t, runtime, agentID, 700+round)
		}
		task, err := api.Claim(ctx, runtime.ID)
		if err != nil || task == nil || task.ID != taskID {
			t.Fatalf("library claim %d: %v", round, err)
		}
		if err = api.Start(ctx, *task); err != nil {
			t.Fatal(err)
		}
		selected, err := opencode.Select(*task)
		if err != nil {
			t.Fatal(err)
		}
		switch round {
		case 0:
			if len(selected) != 1 || selected["public-fixture"].URL != "https://one.example.invalid/mcp?project_ref=fixture" || !strings.Contains(string(task.Agent.MCPConfig), "library-secret-one") {
				t.Fatal("library assignment absent from fresh native claim")
			}
			raw, err := task.TaskToken()
			if err != nil {
				t.Fatal(err)
			}
			nativeAuthorityRequest(t, ctx, base, raw, "POST", path, first, 403)
		case 1:
			if len(selected) != 1 || selected["public-fixture"].URL != "https://two.example.invalid/mcp" || !strings.Contains(string(task.Agent.MCPConfig), "library-secret-two") || strings.Contains(string(task.Agent.MCPConfig), "library-secret-one") {
				t.Fatal("library edit did not reach next native claim")
			}
		case 2:
			if len(selected) != 0 || strings.Contains(string(task.Agent.MCPConfig), "library-secret") {
				t.Fatal("disabled library server still selected")
			}
		}
		encoded, _ := json.Marshal(selected)
		if strings.Contains(string(encoded), "library-secret") {
			t.Fatal("selected MCP credentials formatted")
		}
		if err = api.Complete(ctx, task.ID, execution.Result{}); err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			request("PUT", path+"/"+server.ID, `{"name":"public-fixture","config":{"type":"http","url":"https://two.example.invalid/mcp","headers":{"Authorization":"Bearer library-secret-two"}}}`, 200)
		}
		if round == 1 {
			request("PUT", binding+"/"+server.ID+"/enabled", `{"enabled":false}`, 200)
		}
	}
	request("DELETE", binding+"/"+server.ID, "", 200)
	request("DELETE", path+"/"+server.ID, "", 204)
	t.Log("native library create/assignment/edit/disable reaches next claims; secrets write-only; human-only gate preserved")
}

func queueMCPContinuation(t *testing.T, runtime multica.Runtime, agent string, n int) string {
	id := fmt.Sprintf("20000000-0000-4000-8000-%012d", n)
	sql(t, fmt.Sprintf(`INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','40000000-0000-4000-8000-000000000700','queued',1,'%s','%s');`, id, agent, runtime.ID, user, user))
	return id
}
