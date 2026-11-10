//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

func liveSteeringService(t *testing.T, api *multica.Client) {
	if os.Getenv("VERIFY_STEERING_NATIVE_PATCH") != "1" || os.Getenv("VERIFY_OPENCODE") != "1" || os.Getenv("VERIFY_SERVICE") != "1" {
		t.Skip("requires opt-in native admission patch and real service/OpenCode fixtures")
	}
	var id string
	e := startSteering(t, api, false, func(rt multica.Runtime) {
		id = enqueue(t, rt, 791)
		sql(t, fmt.Sprintf(`UPDATE issue SET title='Live steering fixture' WHERE id='40000000-0000-4000-8000-000000000791';
INSERT INTO comment(id,issue_id,workspace_id,author_type,author_id,content,type) VALUES('d3000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000791','%s','member','%s','LIVE-STEERING-INITIAL','comment');
UPDATE agent_task_queue SET trigger_comment_id='d3000000-0000-4000-8000-000000000001' WHERE id='%s';`, workspace, user, id))
	})
	f, dir, controller, command, c, controllerContainer := e.f, e.dir, e.controller, e.command, e.c, e.container
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("fixture task state: %s", sql(t, fmt.Sprintf("SELECT status||'|'||coalesce(error,'') FROM agent_task_queue WHERE id='%s';", id)))
			t.Logf("fixture controller: %s", dockerTest(t, "logs", "--tail", "40", controllerContainer))
			t.Logf("fixture transcript: %s", sql(t, fmt.Sprintf("SELECT coalesce(json_agg(m ORDER BY seq)::text,'') FROM task_message m WHERE task_id='%s';", id)))
		}
	})
	eventually(t, "initial tool in flight", func() bool { return liveSteeringMarker(t, controller, "/workspace/.steering-initial-active") != "" })
	container := liveSteeringMarker(t, controller, "/workspace/.steering-initial-active")
	session := sql(t, fmt.Sprintf("SELECT coalesce(session_id,'') FROM agent_task_queue WHERE id='%s';", id))
	if !strings.HasPrefix(session, "ses_") {
		t.Fatal("native HTTP session not pinned mid-turn")
	}
	s := &supplements{t: t, api: api, task: multica.Task{ID: id, IssueID: "40000000-0000-4000-8000-000000000791", AgentID: "30000000-0000-4000-8000-000000000791"}}
	s.path = "/api/issues/" + s.task.IssueID + "/tasks/" + id + "/supplements"
	s.request = func(method, path, body string, want int) string {
		return nativeAuthorityRequest(t, context.Background(), os.Getenv("MULTICA_TEST_URL"), token(t), method, path, body, want)
	}
	first := s.create(1, "live-steering-guidance-one", 201)
	if duplicate := s.create(1, "live-steering-guidance-one", 200); duplicate != first {
		t.Fatal("supplement request dedup failed")
	}
	second := s.create(2, "live-steering-guidance-two", 201)
	eventually(t, "first native claim", func() bool {
		return strings.HasPrefix(sql(t, fmt.Sprintf("SELECT status FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", id, first)), "delivering")
	})
	status(t, api, id, "running")
	dockerTest(t, "exec", container, "touch", "/workspace/.steering-release")
	eventually(t, "first guidance applied mid-turn", func() bool { return liveSteeringMarker(t, controller, "/workspace/.steering-first-active") != "" })
	eventually(t, "first delivery receipt", func() bool {
		return sql(t, fmt.Sprintf("SELECT status FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", id, first)) == "delivered"
	})
	status(t, api, id, "running")
	dockerTest(t, "exec", container, "touch", "/workspace/.steering-release-two")
	waitTask(t, api, id, "completed")
	s.receipt(first, "delivered||1")
	s.receipt(second, "delivered||1")
	liveSteeringResult(t, id, session)
	t.Run("clean-resume", func(t *testing.T) { liveSteeringResume(t, api, s, session) })
	t.Run("cancel-restart-isolation", func(t *testing.T) { liveSteeringCancel(t, api, s, 3, controller, nil) })
	t.Run("controller-crash-isolation", func(t *testing.T) { liveSteeringCrash(t, api, s, f, controllerContainer, controller) })
	t.Run("disable-steering-acp", func(t *testing.T) {
		liveSteeringCancel(t, api, s, 5, controller, func() {
			c.OpenCode.LiveSteering = false
			c.Command = []string{"/bin/sh", "-c", strings.Replace(command, "opencode serve --stdio --hostname 127.0.0.1 --port 4096", "opencode acp", 1)}
			steeringSwitchToACP(t, f, controllerContainer, filepath.Join(dir, "config.json"), c)
		})
	})
	t.Log("same native task/session consumed ordered supplements before original completion; duplicate creation deduplicated; canonical transcript and usage retained")
}

func steeringFollowup(t *testing.T, s *supplements, content string) string {
	t.Helper()
	raw := s.request("POST", "/api/issues/"+s.task.IssueID+"/comments", fmt.Sprintf(`{"content":"[@Fixture](mention://agent/%s) %s"}`, s.task.AgentID, content), 201)
	var comment struct{ ID string }
	if json.Unmarshal([]byte(raw), &comment) != nil || comment.ID == "" {
		t.Fatal("follow-up comment missing ID")
	}
	var id string
	eventually(t, "native follow-up task", func() bool {
		id = sql(t, fmt.Sprintf("SELECT id FROM agent_task_queue WHERE trigger_comment_id='%s';", comment.ID))
		return multica.ValidID(id)
	})
	return id
}

func liveSteeringResume(t *testing.T, api *multica.Client, s *supplements, original string) {
	s = steeringScope(t, s)
	id := steeringFollowup(t, s, "live-steering-resume-check")
	waitTask(t, api, id, "completed")
	session := sql(t, fmt.Sprintf("SELECT session_id FROM agent_task_queue WHERE id='%s';", id))
	if session != original {
		t.Fatalf("clean HTTP owner did not resume session: %s != %s", session, original)
	}
	if result := sql(t, fmt.Sprintf("SELECT result::text FROM agent_task_queue WHERE id='%s';", id)); !strings.Contains(result, "Resumed steering guidance one two") {
		t.Fatalf("retained guidance missing: %s", result)
	}
}

func liveSteeringCancel(t *testing.T, api *multica.Client, s *supplements, requestID int, controller string, beforeResume func()) {
	s = steeringScope(t, s)
	id := steeringFollowup(t, s, "live-steering-cancel-check")
	eventually(t, "cancellation tool active", func() bool { return liveSteeringMarker(t, controller, "/workspace/.steering-cancel-active") != "" })
	original := sql(t, fmt.Sprintf("SELECT session_id FROM agent_task_queue WHERE id='%s';", id))
	cancelled := *s
	cancelled.task.ID, cancelled.path = id, "/api/issues/"+s.task.IssueID+"/tasks/"+id+"/supplements"
	comment := cancelled.create(requestID, "live-steering-guidance-never", 201)
	eventually(t, "cancellation-race claim", func() bool {
		return sql(t, fmt.Sprintf("SELECT status FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", id, comment)) == "delivering"
	})
	s.request("POST", "/api/tasks/"+id+"/cancel", `{}`, 200)
	waitTask(t, api, id, "cancelled")
	eventually(t, "cancelled workload cleanup", func() bool {
		return strings.TrimSpace(dockerTest(t, "ps", "-q", "--filter", "label=io.multica-sandbox.owner="+controller)) == ""
	})
	cancelled.receipt(comment, "failed|turn_ended|1")
	if beforeResume != nil {
		beforeResume()
	}
	fresh := steeringFollowup(t, s, "live-steering-resume-check")
	waitTask(t, api, fresh, "completed")
	session := sql(t, fmt.Sprintf("SELECT session_id FROM agent_task_queue WHERE id='%s';", fresh))
	if session == original {
		t.Fatal("interrupted HTTP DB resumed automatically under a new attempt")
	}
	result := sql(t, fmt.Sprintf("SELECT result::text FROM agent_task_queue WHERE id='%s';", fresh))
	if strings.Contains(result, "STALE-GUIDANCE") || !strings.Contains(result, "Fresh after interrupted owner steering guidance one two") {
		t.Fatalf("interrupted owner continuity/steering isolation lost: %s", result)
	}
}

func steeringScope(t *testing.T, s *supplements) *supplements {
	child := *s
	child.t = t
	child.request = func(method, path, body string, want int) string {
		return nativeAuthorityRequest(t, context.Background(), os.Getenv("MULTICA_TEST_URL"), token(t), method, path, body, want)
	}
	return &child
}

func liveSteeringMarker(t *testing.T, controller, marker string) string {
	return liveSteeringTaskMarker(t, controller, marker, "")
}

func liveSteeringTaskMarker(t *testing.T, controller, marker, task string) string {
	t.Helper()
	containers := dockerTest(t, "ps", "--filter", "label=io.multica-sandbox.owner="+controller, "--format", "{{.ID}}")
	for _, container := range strings.Fields(containers) {
		if _, err := execOutput(container, `test -f "/proc/1/root$1" || exit 1
[ -z "$2" ] && exit 0
for d in /proc/[0-9]*; do
 if tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qFx "MULTICA_TASK_ID=$2"; then exit 0; fi
done
exit 1`, marker, task); err == nil {
			return container
		}
	}
	return ""
}

func liveSteeringResult(t *testing.T, id, session string) {
	t.Helper()
	result := sql(t, fmt.Sprintf("SELECT coalesce(session_id,'')||'|'||result::text FROM agent_task_queue WHERE id='%s';", id))
	if !strings.HasPrefix(result, session+"|") || !strings.Contains(result, "STEERING-ORDER one two") {
		t.Fatalf("steering result/session: %s", result)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE issue_id='40000000-0000-4000-8000-000000000791';")); got != "1" {
		t.Fatalf("steering spawned another native task: %s", got)
	}
	retained := sql(t, fmt.Sprintf("SELECT session_rollout_missing::text||'|'||coalesce(work_dir,'') FROM agent_task_queue WHERE id='%s';", id))
	if !strings.HasPrefix(retained, "false|multica-sandbox-session-") {
		t.Fatalf("steering session not retained: %s", retained)
	}
	pairs := sql(t, fmt.Sprintf("SELECT count(*) FROM task_message c JOIN task_message r ON r.task_id=c.task_id AND r.call_id=c.call_id AND r.seq=c.seq+1 WHERE c.task_id='%s' AND c.type='tool_use' AND r.type='tool_result';", id))
	if pairs != "3" {
		t.Fatalf("steering tool call/result correlation lost: %s", pairs)
	}
	usage := sql(t, fmt.Sprintf("SELECT input_tokens||'|'||output_tokens FROM task_usage WHERE task_id='%s';", id))
	if usage != "44|28" {
		t.Fatalf("steering usage not accumulated once: %s", usage)
	}
	ordered := sql(t, fmt.Sprintf("SELECT count(*)=count(DISTINCT seq) AND min(seq)=1 AND max(seq)=count(*) FROM task_message WHERE task_id='%s';", id))
	if ordered != "t" {
		t.Fatal("steering transcript sequence has gaps or duplicates")
	}
}

// steeringEnv is a real controller with the owned OpenCode HTTP transport against
// the patched disposable server, optionally behind the fault proxy.
type steeringEnv struct {
	f                                   serviceFixture
	c                                   service.Config
	dir, controller, command, container string
	// proxy is the fault proxy's control address on the host, empty without faults.
	proxy string
	rt    multica.Runtime
}

func startSteering(t *testing.T, api *multica.Client, withFaults bool, prepare func(multica.Runtime)) *steeringEnv {
	t.Helper()
	dir, stamp := t.TempDir(), time.Now().UnixNano()
	project := fmt.Sprintf("sandbox-steering-%d", stamp)
	server := os.Getenv("MULTICA_TEST_SERVER_CONTAINER")
	network := strings.TrimSuffix(server, "-server")
	override := filepath.Join(dir, "fixture.json")
	services := map[string]any{"seed": map[string]any{"environment": map[string]string{"ROTATION_FIXTURE": "1"}}, "provider": providerService()}
	targets, origin := []string{"gateway", "provider"}, "http://"+server+":8080"
	if withFaults {
		proxy := providerService()
		proxy["command"], proxy["networks"], proxy["ports"] = []string{"fault-proxy"}, []string{"fixture"}, []string{"127.0.0.1::8080"}
		proxy["environment"] = map[string]string{"UPSTREAM": origin}
		services["faults"], targets, origin = proxy, append(targets, "faults"), "http://"+project+"-faults-1:8080"
	}
	writeJSON(t, override, map[string]any{"services": services, "networks": map[string]any{"fixture": map[string]any{"external": true, "name": network, "internal": nil}, "execution": map[string]any{"internal": true}}})
	startFixture(t, project, override, targets)
	e := &steeringEnv{f: serviceFixture{fmt.Sprintf("sandbox-steering-controller-%d", stamp), filepath.Join(dir, "controller.json"), t}, dir: dir}
	if withFaults {
		e.proxy = "http://127.0.0.1:" + strings.TrimSpace(strings.SplitN(dockerTest(t, "port", project+"-faults-1", "8080/tcp"), ":", 2)[1])
	}
	e.controller = fmt.Sprintf("d1000000-0000-4000-8000-%012x", uint64(stamp)&0xffffffffffff)
	t.Cleanup(func() { cleanupSteeringSessions(t, e.controller) })
	e.command = `OPENCODE_CONFIG_CONTENT='{"model":"fixture/fixture","enabled_providers":["fixture"],"provider":{"fixture":{"npm":"@ai-sdk/openai-compatible","name":"Fixture","options":{"baseURL":"http://provider:8080/v1"},"models":{"fixture":{"name":"Fixture","limit":{"context":64000,"output":4096}}}}}}' exec opencode serve --stdio --hostname 127.0.0.1 --port 4096`
	e.c = service.Config{Server: origin, AllowHTTP: true, Daemon: e.controller, Image: officialImage,
		Command: []string{"/bin/sh", "-c", e.command}, Timeout: "180s", OpenCode: &service.OpenCodeConfig{LiveSteering: true, Network: project + "_execution", Peers: []string{project + "-provider-1", e.f.project + "-controller-1"},
			MulticaRelay: &service.RelayConfig{Listen: ":8091", URL: "http://multica-relay:8091"}, MulticaCLI: os.Getenv("MULTICA_TEST_CLI_IMAGE"), Helper: os.Getenv("MULTICA_TEST_HELPER_IMAGE"), Sessions: &service.SessionsConfig{TTL: "1h", Max: 8}}}
	writeJSON(t, filepath.Join(dir, "identity.json"), map[string]any{})
	rt, err := api.RegisterProvider(context.Background(), workspace, e.controller, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	e.rt = rt
	prepare(rt)
	prepareManagedService(t, e.f, dir, e.c, project, network)
	e.container = e.f.compose("ps", "-q", "controller")
	controllerDiagnostics(t, e.container)
	eventually(t, "steering runtime registration", func() bool { return strings.Contains(dockerTest(t, "logs", e.container), readyEvent) })
	return e
}
