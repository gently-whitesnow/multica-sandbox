//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

type authorityRequest func(method, path, body string, want int) string

// Authorization belongs to Multica: the relay preserves the native task actor and
// its runtime owner's workspace role, including admin authority when configured.
func taskAuthority(t *testing.T, api *multica.Client, rt multica.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sql(t, fmt.Sprintf("UPDATE member SET role='admin' WHERE workspace_id='%s' AND user_id='%s';", workspace, user))
	defer sql(t, fmt.Sprintf("UPDATE member SET role='owner' WHERE workspace_id='%s' AND user_id='%s';", workspace, user))
	id := enqueue(t, rt, 50)
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("admin claim: %v", err)
	}
	if err = api.Start(ctx, *task); err != nil {
		t.Fatal(err)
	}
	raw, err := task.TaskToken()
	if err != nil {
		t.Fatal(err)
	}
	origin := os.Getenv("MULTICA_TEST_URL")
	grants := relay.NewGrants(multica.RelayPrefix)
	opaque, err := grants.Issue(task.AttemptKey(), relay.Upstream{Origin: origin, Credential: raw})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := relay.New(grants, relay.Policy{Allow: multica.RelayPath, Limit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	request := func(method, path, body string, want int) string {
		return nativeAuthorityRequest(t, ctx, proxy.URL, opaque, method, path, body, want)
	}
	workspacePath := "/api/workspaces/" + workspace
	request("PATCH", workspacePath, `{"name":"Native admin fixture"}`, 200)
	sql(t, fmt.Sprintf("UPDATE member SET role='member' WHERE workspace_id='%s' AND user_id='%s';", workspace, user))
	request("PATCH", workspacePath, `{"name":"must not change"}`, 403)
	if got := sql(t, fmt.Sprintf("SELECT name FROM workspace WHERE id='%s';", workspace)); got != "Native admin fixture" {
		t.Fatal("upstream member role did not protect workspace settings")
	}
	nativeProjectWorkflow(t, request)
	sql(t, fmt.Sprintf("UPDATE member SET role='admin' WHERE workspace_id='%s' AND user_id='%s';", workspace, user))
	nativeTaskOperations(t, request, *task)
	nativeActorRestrictions(t, ctx, origin, raw, request, *task)
	grants.Revoke(task.AttemptKey())
	request("GET", "/api/issues/"+task.IssueID, "", 401)
	if err = api.Complete(ctx, id, execution.Result{}); err != nil {
		t.Fatal(err)
	}
	t.Log("native admin/member authorization, project/workflow writes, agent actor, upstream denials and ended grant verified")
}

func nativeAuthorityRequest(t *testing.T, ctx context.Context, base, credential, method, path, body string, want int) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workspace-ID", workspace)
	// Multica derives actor identity from mat_, overwriting these forged headers.
	req.Header.Set("X-Actor-Source", "member")
	req.Header.Set("X-Agent-ID", user)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	responseBody, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d: %s", method, path, res.StatusCode, want, responseBody)
	}
	return string(responseBody)
}

func nativeProjectWorkflow(t *testing.T, request authorityRequest) {
	t.Helper()
	body := request("POST", "/api/projects", `{"title":"Native project fixture","resources":[{"resource_type":"github_repo","resource_ref":{"url":"https://github.com/example/fixture","ref":"main"}}]}`, 201)
	var project struct{ ID string }
	if json.Unmarshal([]byte(body), &project) != nil || !multica.ValidID(project.ID) {
		t.Fatal("project creation did not return its ID")
	}
	projectPath := "/api/projects/" + project.ID
	request("PUT", projectPath, `{"description":"Created by a native member task"}`, 200)
	if !strings.Contains(request("GET", projectPath+"/resources", "", 200), "https://github.com/example/fixture") {
		t.Fatal("project repository was not persisted")
	}
	body = request("POST", "/api/issue-workflows", `{"name":"Native workflow fixture","steps":[{"status_key":"backlog"},{"status_key":"in_progress"},{"status_key":"blocked"}]}`, 201)
	var workflow struct{ ID string }
	if json.Unmarshal([]byte(body), &workflow) != nil || !multica.ValidID(workflow.ID) {
		t.Fatal("workflow creation did not return its ID")
	}
	workflowPath := "/api/issue-workflows/" + workflow.ID
	request("GET", "/api/issue-workflows", "", 200)
	request("PATCH", workflowPath, `{"description":"Native workflow owner update"}`, 200)
	if !strings.Contains(request("GET", workflowPath, "", 200), "Native workflow owner update") {
		t.Fatal("workflow owner update was not persisted")
	}
}

func nativeTaskOperations(t *testing.T, request authorityRequest, task multica.Task) {
	t.Helper()
	request("GET", "/api/issues/"+task.IssueID, "", 200)
	request("GET", "/api/issues/"+task.IssueID+"?workspace_id=b7000000-0000-4000-8000-000000000001", "", 403)
	request("POST", "/api/issues/"+task.IssueID+"/comments", `{"content":"Native authority fixture comment"}`, 201)
	if got := sql(t, fmt.Sprintf("SELECT author_type FROM comment WHERE issue_id='%s' AND content='Native authority fixture comment';", task.IssueID)); got != "agent" {
		t.Fatalf("comment actor=%q", got)
	}
	wakeupPath := "/api/issues/" + task.IssueID + "/wakeups"
	request("POST", wakeupPath, `{"instruction":"Fixture deferred continuation","kind":"at","after_seconds":3600}`, 201)
	request("GET", wakeupPath, "", 200)
	wakeup := sql(t, fmt.Sprintf("SELECT id FROM issue_wakeup WHERE issue_id='%s' AND instruction='Fixture deferred continuation';", task.IssueID))
	if !multica.ValidID(wakeup) {
		t.Fatal("task wakeup was not persisted")
	}
	request("POST", wakeupPath+"/"+wakeup+"/disable", `{}`, 200)
}

func nativeActorRestrictions(t *testing.T, ctx context.Context, origin, raw string, request authorityRequest, task multica.Task) {
	t.Helper()
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/agents/" + task.AgentID + "/env", ""},
		{"POST", "/api/tokens", `{"name":"must not mint"}`},
		{"POST", "/api/cli-token", `{}`},
	} {
		direct := nativeAuthorityRequest(t, ctx, origin, raw, tc.method, tc.path, tc.body, 403)
		if got := request(tc.method, tc.path, tc.body, 403); got != direct {
			t.Fatal("relay did not preserve Multica's native agent rejection")
		}
	}
	request("GET", "/api/future-administration", "", 404)
	sql(t, fmt.Sprintf(`UPDATE agent SET custom_env='{"fixture_secret":"private-env-sentinel"}',mcp_config='{"mcpServers":{"fixture":{"url":"https://example.invalid/mcp","headers":{"Authorization":"private-mcp-sentinel"}}}}' WHERE id='%s';`, task.AgentID))
	body := request("GET", "/api/agents/"+task.AgentID, "", 200)
	if strings.Contains(body, "private-env-sentinel") || strings.Contains(body, "private-mcp-sentinel") {
		t.Fatal("Multica did not redact native agent reads")
	}
}
