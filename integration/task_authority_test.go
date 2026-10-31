//go:build upstream

package integration

import (
	"context"
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

// A real administrative runtime owner mints an owner-scoped mat_ token upstream.
// The sandbox must retain that token outside and expose only the member task API.
func taskAuthority(t *testing.T, api *multica.Client, rt multica.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	handler, err := relay.New(grants, relay.Policy{Allow: multica.RelayPath, AllowRequest: multica.RelayRequest, Limit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	request := func(base, credential, method, path, body string, want int) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", workspace)
		// The server overwrites actor identity from the task token, not these headers.
		req.Header.Set("X-Actor-Source", "member")
		req.Header.Set("X-Agent-ID", user)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		responseBody, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d", method, path, res.StatusCode, want)
		}
		return string(responseBody)
	}
	workspacePath := "/api/workspaces/" + workspace
	// Establish that the pinned server would grant admin authority to this exact
	// task token; the following refusals cannot pass just because upstream denies it.
	request(origin, raw, "PATCH", workspacePath, `{"name":"Contract fixture"}`, 200)
	sql(t, fmt.Sprintf("UPDATE member SET role='member' WHERE workspace_id='%s' AND user_id='%s';", workspace, user))
	request(origin, raw, "PATCH", workspacePath, `{"name":"must not change"}`, 403)
	sql(t, fmt.Sprintf("UPDATE member SET role='admin' WHERE workspace_id='%s' AND user_id='%s';", workspace, user))
	for _, tc := range []struct{ method, path, body string }{
		{"PATCH", workspacePath, `{"name":"must not change"}`},
		{"PATCH", workspacePath + "/members/" + user, `{"role":"owner"}`},
		{"GET", workspacePath + "/env", ""},
		{"PUT", "/api/agents/" + task.AgentID, `{"instructions":"must not change"}`},
		{"GET", "/api/agents/" + task.AgentID + "/env", ""},
		{"POST", "/api/tokens", `{"name":"must not mint"}`},
		{"POST", "/api/daemon/register", `{}`},
		{"POST", workspacePath + "/mcp-servers", `{}`},
		{"GET", "/api/future-administration", ""},
	} {
		request(proxy.URL, opaque, tc.method, tc.path, tc.body, 403)
	}
	request(proxy.URL, opaque, "GET", "/api/issues/"+task.IssueID, "", 200)
	request(proxy.URL, opaque, "GET", "/api/issues/"+task.IssueID+"?workspace_id=b7000000-0000-4000-8000-000000000001", "", 403)
	request(proxy.URL, opaque, "POST", "/api/issues/"+task.IssueID+"/comments", `{"content":"Capability fixture comment"}`, 201)
	if got := sql(t, fmt.Sprintf("SELECT author_type FROM comment WHERE issue_id='%s' AND content='Capability fixture comment';", task.IssueID)); got != "agent" {
		t.Fatalf("comment actor=%q", got)
	}
	wakeupPath := "/api/issues/" + task.IssueID + "/wakeups"
	request(proxy.URL, opaque, "POST", wakeupPath, `{"instruction":"Fixture deferred continuation","kind":"at","after_seconds":3600}`, 201)
	request(proxy.URL, opaque, "GET", wakeupPath, "", 200)
	wakeup := sql(t, fmt.Sprintf("SELECT id FROM issue_wakeup WHERE issue_id='%s' AND instruction='Fixture deferred continuation';", task.IssueID))
	if !multica.ValidID(wakeup) {
		t.Fatal("task wakeup was not persisted")
	}
	request(proxy.URL, opaque, "POST", wakeupPath+"/"+wakeup+"/disable", `{}`, 200)
	sql(t, fmt.Sprintf(`UPDATE agent SET custom_env='{"fixture_secret":"private-env-sentinel"}',mcp_config='{"mcpServers":{"fixture":{"url":"https://example.invalid/mcp","headers":{"Authorization":"private-mcp-sentinel"}}}}' WHERE id='%s';`, task.AgentID))
	body := request(proxy.URL, opaque, "GET", "/api/agents/"+task.AgentID, "", 200)
	if strings.Contains(body, "private-env-sentinel") || strings.Contains(body, "private-mcp-sentinel") {
		t.Fatal("admin controller leaked agent credentials through member reads")
	}
	grants.Revoke(task.AttemptKey())
	request(proxy.URL, opaque, "GET", "/api/issues/"+task.IssueID, "", 401)
	if err = api.Complete(ctx, id, execution.Result{}); err != nil {
		t.Fatal(err)
	}
	t.Log("admin controller claimed and completed; member task operations succeed, administrative routes and ended grant refused")
}
