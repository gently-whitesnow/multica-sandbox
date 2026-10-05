//go:build upstream

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func modelRequest(t *testing.T, method, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, os.Getenv("MULTICA_TEST_URL")+path, bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token(t))
	req.Header.Set("X-Workspace-ID", workspace)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("fixture API HTTP %d", response.StatusCode)
	}
	var out map[string]any
	if json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&out) != nil {
		t.Fatal("invalid fixture API response")
	}
	return out
}
func upstreamDiscoveryGap(t *testing.T, runtime string) {
	result := modelRequest(t, "POST", "/api/runtimes/"+runtime+"/models?force=true")
	id, ok := result["id"].(string)
	if !ok {
		t.Fatal("discovery request missing")
	}
	eventually(t, "explicit unscoped discovery failure", func() bool {
		result = modelRequest(t, "GET", "/api/runtimes/"+runtime+"/models/"+id)
		return result["status"] == "failed"
	})
	message, _ := result["error"].(string)
	if !strings.Contains(message, "agent_id") || result["models"] != nil {
		t.Fatal("upstream gap hid failure or disclosed an unscoped catalog")
	}
	t.Log("unmodified upstream discovery reports the missing agent scope; no union catalog is published")
}
func controllerModelSelections(t *testing.T, api *multica.Client, runtime, agent, project string) {
	upstreamDiscoveryGap(t, runtime)
	for i, model := range []string{"fixture-new", "ungranted", "fixture-limited"} {
		id := fmt.Sprintf("70000000-0000-4000-8000-%012d", i+70)
		issue := fmt.Sprintf("80000000-0000-4000-8000-%012d", i+70)
		sql(t, fmt.Sprintf(`UPDATE agent SET model='managed-inference/%s',thinking_level='high',instructions='Reply briefly.',mcp_config='{}' WHERE id='%s';
 INSERT INTO issue(id,workspace_id,title,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','Model fixture','in_progress','member','%s',%d,'agent','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s');`, model, agent, issue, workspace, user, 100070+i, agent, id, agent, runtime, issue, user, user))
		if model == "fixture-limited" {
			eventually(t, "gateway budget refusal", func() bool { return selectionEvidence(t, project, id, model, "high", false) })
			modelRequest(t, "POST", "/api/tasks/"+id+"/cancel")
			waitTask(t, api, id, "cancelled")
		} else {
			want := "completed"
			if model == "ungranted" {
				want = "failed"
			}
			waitTask(t, api, id, want)
			if model == "ungranted" && !strings.Contains(sql(t, fmt.Sprintf("SELECT error FROM agent_task_queue WHERE id='%s';", id)), "HTTP 403") {
				t.Fatal("gateway refusal status not surfaced in Multica")
			}
		}
		if !selectionEvidence(t, project, id, model, "high", true) {
			t.Fatal("selected model/effort did not reach gateway")
		}
	}
	t.Log("real Multica selected model/reasoning reaches native OpenCode/LiteLLM; stale catalog permits explicit selection, refusals do not substitute models")
}
func selectionEvidence(t *testing.T, project, id, model, effort string, assertNoFallback bool) bool {
	t.Helper()
	var evidence map[string][2]int
	if json.Unmarshal([]byte(dockerTest(t, "exec", project+"-gateway-1", "/identity-example", "evidence")), &evidence) != nil {
		t.Fatal("invalid selection evidence")
	}
	found := false
	for key, count := range evidence {
		if !strings.HasPrefix(key, "selection:") || !strings.Contains(key, ":"+id+":") {
			continue
		}
		if strings.HasSuffix(key, "|"+model+"|"+effort) && count[0] > 0 {
			found = true
		} else if assertNoFallback {
			t.Fatal("native adapter silently substituted model or effort")
		}
	}
	return found
}
