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

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const supplementCapability = "task-supplement-v1"

type supplements struct {
	t       *testing.T
	api     *multica.Client
	task    multica.Task
	request authorityRequest
	path    string
}

// This is native protocol conformance, not an OpenCode implementation. The
// supported reference provider is seeded only in the disposable fixture DB.
func taskSupplements(t *testing.T, api *multica.Client) {
	t.Run("opencode-unsupported", func(t *testing.T) {
		s := supplementFixture(t, api, 71, "opencode", true)
		s.create(1, "unsupported", 412)
		comment := s.request("POST", "/api/issues/"+s.task.IssueID+"/comments", fmt.Sprintf(`{"content":"[@Fixture](mention://agent/%s) Follow-up fixture","steer_task_ids":[%q]}`, s.task.AgentID, s.task.ID), 201)
		if strings.Contains(comment, `"supplement_status":"pending"`) {
			t.Fatal("unsupported OpenCode run advertised a pending supplement")
		}
		var response struct {
			ID       string
			Outcomes []struct{ Status string } `json:"trigger_outcomes"`
		}
		if json.Unmarshal([]byte(comment), &response) != nil || len(response.Outcomes) != 1 || response.Outcomes[0].Status != "queued" {
			t.Fatalf("unsupported steering must keep native queued handling: %s", comment)
		}
		if got := sql(t, fmt.Sprintf("SELECT count(*) FROM task_supplement WHERE task_id='%s';", s.task.ID)); got != "0" {
			t.Fatalf("unsupported provider persisted supplements: %s", got)
		}
		s.finish()
		if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id='%s' AND status='queued';", response.ID)); got != "1" {
			t.Fatalf("native fallback did not queue the follow-up after completion: %s", got)
		}
	})
	t.Run("missing-capability", func(t *testing.T) {
		s := supplementFixture(t, api, 72, "codex", false)
		s.create(1, "unsupported", 412)
		s.finish()
	})
	t.Run("claim-ack-retry", func(t *testing.T) { supplementDelivery(t, api) })
	t.Run("terminal-settlement", func(t *testing.T) { supplementTerminal(t, api) })
	t.Run("cancel-recovery", func(t *testing.T) { supplementRecovery(t, api) })
	t.Run("ownership", func(t *testing.T) { supplementOwnership(t, api) })
	t.Run("receipt-inspection-absent", func(t *testing.T) { supplementInspectionAbsent(t, api) })
}

// Native v0.7.2 has no bounded daemon receipt read. The sandbox then does not
// offer steering at all, even for a provider the server would negotiate.
func supplementInspectionAbsent(t *testing.T, api *multica.Client) {
	if os.Getenv("VERIFY_STEERING_NATIVE_PATCH") == "1" {
		t.Skip("the fixture patch adds receipt inspection")
	}
	rt, err := api.RegisterProvider(context.Background(), workspace, "d0000000-0000-4000-8000-000000000081", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	sql(t, fmt.Sprintf("UPDATE agent_runtime SET provider='codex' WHERE id='%s';", rt.ID))
	id := enqueue(t, rt, 81)
	task, err := api.Claim(context.Background(), rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("inspection fixture claim: %v", err)
	}
	nativeAuthorityRequest(t, context.Background(), os.Getenv("MULTICA_TEST_URL"), token(t), "GET", "/api/daemon/tasks/"+id+"/supplements", "", 404)
	client, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.UseSupplements(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err = client.Start(context.Background(), *task); err != nil || client.SupplementsNegotiated(*task) {
		t.Fatalf("steering negotiated without native receipt inspection: %v", err)
	}
	if got := sql(t, fmt.Sprintf("SELECT status||'|'||(SELECT count(*) FROM task_supplement_capability WHERE task_id='%s') FROM agent_task_queue WHERE id='%s';", id, id)); got != "running|0" {
		t.Fatalf("unsupported server recorded a supplement capability: %s", got)
	}
	if err = api.AgentComplete(context.Background(), id, execution.Result{Output: "Fixture finished"}); err != nil {
		t.Fatal(err)
	}
}

func supplementFixture(t *testing.T, api *multica.Client, n int, provider string, offer bool) *supplements {
	return supplementAdmissionFixture(t, api, n, provider, offer, offer && provider == "codex")
}

func supplementAdmissionFixture(t *testing.T, api *multica.Client, n int, provider string, offer, negotiated bool) *supplements {
	t.Helper()
	rt, err := api.RegisterProvider(context.Background(), workspace, fmt.Sprintf("d0000000-0000-4000-8000-%012d", n), "opencode")
	if err != nil {
		t.Fatal(err)
	}
	sql(t, fmt.Sprintf("UPDATE agent_runtime SET provider='%s' WHERE id='%s';", provider, rt.ID))
	id := enqueue(t, rt, n)
	task, err := api.Claim(context.Background(), rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("supplement fixture claim: %v", err)
	}
	s := &supplements{t: t, api: api, task: *task, path: "/api/issues/" + task.IssueID + "/tasks/" + task.ID + "/supplements"}
	s.request = func(method, path, body string, want int) string {
		return nativeAuthorityRequest(t, context.Background(), os.Getenv("MULTICA_TEST_URL"), token(t), method, path, body, want)
	}
	caps := []string{}
	if offer {
		caps = append(caps, supplementCapability)
	}
	body, _ := json.Marshal(map[string]any{"runtime_id": rt.ID, "dispatched_at": task.DispatchedAt, "capabilities": caps})
	response := s.request("POST", "/api/daemon/tasks/"+id+"/start", string(body), 200)
	var start struct {
		SupplementCapability string `json:"supplement_capability"`
	}
	if json.Unmarshal([]byte(response), &start) != nil || (start.SupplementCapability == supplementCapability) != negotiated {
		t.Fatalf("native supplement negotiation provider=%s offered=%v: %s", provider, offer, response)
	}
	return s
}

func (s *supplements) create(n int, content string, want int) string {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"content": content, "client_request_id": fmt.Sprintf("a0000000-0000-4000-8000-%012d", n)})
	response := s.request("POST", s.path, string(body), want)
	if want >= 400 {
		return ""
	}
	var comment struct{ ID string }
	if json.Unmarshal([]byte(response), &comment) != nil || !multica.ValidID(comment.ID) {
		s.t.Fatal("supplement creation missing comment ID")
	}
	return comment.ID
}

func (s *supplements) claim() string {
	s.t.Helper()
	response := s.request("POST", "/api/daemon/tasks/"+s.task.ID+"/supplements/claim", `{"capabilities":["task-supplement-author-v1"]}`, 200)
	var message struct {
		CommentID string `json:"comment_id"`
	}
	if json.Unmarshal([]byte(response), &message) != nil {
		s.t.Fatal("invalid claim response")
	}
	return message.CommentID
}

func (s *supplements) ack(comment string, delivered bool, reason string, want int) {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"delivered": delivered, "error": reason})
	s.request("POST", "/api/daemon/tasks/"+s.task.ID+"/supplements/"+comment+"/ack", string(body), want)
}

func (s *supplements) receipt(comment, want string) {
	s.t.Helper()
	got := sql(s.t, fmt.Sprintf("SELECT status||'|'||coalesce(failure_reason,'')||'|'||attempt_count FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", s.task.ID, comment))
	if got != want {
		s.t.Fatalf("supplement receipt=%q, want %q", got, want)
	}
}

func (s *supplements) finish() {
	s.t.Helper()
	if err := s.api.AgentComplete(context.Background(), s.task.ID, execution.Result{Output: "Fixture finished"}); err != nil {
		s.t.Fatal(err)
	}
}

func supplementDelivery(t *testing.T, api *multica.Client) {
	s := supplementFixture(t, api, 73, "codex", true)
	first := s.create(1, "first", 201)
	if again := s.create(1, "first", 200); again != first {
		t.Fatal("client request replay created another comment")
	}
	second := s.create(2, "second", 201)
	loseSupplementResponse(t, "/api/daemon/tasks/"+s.task.ID+"/supplements/claim", `{}`)
	// Native v0.7.2 has no delivery lease: after a lost successful claim
	// response, the next claim advances rather than redelivering the first.
	if got := s.claim(); got != second {
		t.Fatalf("lost-response follow-up=%s, want second=%s", got, second)
	}
	s.receipt(first, "delivering||1")
	s.ack(second, false, "private provider diagnostic", 200)
	s.receipt(second, "failed|provider_rejected|1")
	s.ack(second, false, "timeout", 409)
	s.request("POST", s.path+"/"+second+"/retry", `{}`, 200)
	s.request("POST", s.path+"/"+second+"/retry", `{}`, 200)
	if got := s.claim(); got != second {
		t.Fatal("explicit native retry did not requeue the failed message")
	}
	loseSupplementResponse(t, "/api/daemon/tasks/"+s.task.ID+"/supplements/"+second+"/ack", `{"delivered":true}`)
	s.ack(second, true, "", 200)
	s.receipt(second, "delivered||2")
	if got := s.claim(); got != "" {
		t.Fatal("empty queue redelivered a claimed message")
	}
	s.finish()
	s.receipt(first, "failed|turn_ended|1")
	s.receipt(second, "delivered||2")
}

// loseSupplementResponse commits the real native HTTP request, drains its reply,
// then disconnects the fixture client before any response headers reach it.
func loseSupplementResponse(t *testing.T, path, body string) {
	t.Helper()
	status := make(chan int, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, err := http.NewRequestWithContext(r.Context(), "POST", os.Getenv("MULTICA_TEST_URL")+path, strings.NewReader(body))
		if err != nil {
			status <- 0
			return
		}
		request.Header.Set("Authorization", "Bearer "+token(t))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			status <- 0
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		status <- response.StatusCode
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer proxy.Close()
	response, err := http.Post(proxy.URL, "application/json", strings.NewReader(body))
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("fixture did not lose the successful response")
	}
	if got := <-status; got != 200 {
		t.Fatalf("lost-response native request status=%d", got)
	}
}

func supplementTerminal(t *testing.T, api *multica.Client) {
	s := supplementFixture(t, api, 74, "codex", true)
	claimed, pending := s.create(1, "accepted before completion", 201), s.create(2, "not yet claimed", 201)
	if got := s.claim(); got != claimed {
		t.Fatal("unexpected terminal-race claim")
	}
	other := supplementFixture(t, api, 75, "codex", true)
	other.ack(claimed, true, "", 409)
	other.request("POST", strings.Replace(s.path, s.task.IssueID, other.task.IssueID, 1), `{"content":"wrong issue","client_request_id":"a0000000-0000-4000-8000-000000000009"}`, 404)
	other.finish()
	s.finish()
	s.receipt(claimed, "failed|turn_ended|1")
	s.receipt(pending, "failed|turn_ended|0")
	s.ack(claimed, true, "", 200)
	s.receipt(claimed, "delivered||1")
	s.ack(pending, true, "", 409)
	s.request("POST", s.path+"/"+pending+"/retry", `{}`, 409)
	s.create(3, "too late", 409)
	if got := s.claim(); got != "" {
		t.Fatal("ended task claimed another message")
	}
}

func supplementRecovery(t *testing.T, api *multica.Client) {
	for n, action := range []string{"cancel", "recover"} {
		s := supplementFixture(t, api, 76+n, "codex", true)
		comment := s.create(1, "pending lifecycle guidance", 201)
		if s.claim() != comment {
			t.Fatal("lifecycle claim mismatch")
		}
		if action == "cancel" {
			s.request("POST", "/api/tasks/"+s.task.ID+"/cancel", `{}`, 200)
			if err := api.CancelAck(context.Background(), s.task.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			sql(t, fmt.Sprintf("UPDATE agent_task_queue SET max_attempts=2 WHERE id='%s';", s.task.ID))
			recovered, err := api.Recover(context.Background(), s.task.RuntimeID)
			if err != nil || recovered.Orphaned != 1 || recovered.Retried != 1 {
				t.Fatalf("supplement recovery: %+v %v", recovered, err)
			}
			retry, err := api.Claim(context.Background(), s.task.RuntimeID)
			if err != nil || retry == nil || retry.ID == s.task.ID {
				t.Fatalf("fresh retry claim: %v", err)
			}
			if err := api.Start(context.Background(), *retry); err != nil {
				t.Fatal(err)
			}
			prefix := "/api/daemon/tasks/" + retry.ID + "/supplements"
			s.request("POST", prefix+"/claim", `{}`, 200)
			s.request("POST", prefix+"/"+comment+"/ack", `{"delivered":true}`, 409)
			if got := sql(t, fmt.Sprintf("SELECT count(*) FROM task_supplement WHERE task_id='%s';", retry.ID)); got != "0" {
				t.Fatal("retry inherited the prior attempt's supplement")
			}
			if err := api.AgentComplete(context.Background(), retry.ID, execution.Result{Output: "Retry fixture finished"}); err != nil {
				t.Fatal(err)
			}
		}
		s.receipt(comment, "failed|turn_ended|1")
		if s.claim() != "" {
			t.Fatal("terminal task retained claimable supplements")
		}
	}
}

func supplementOwnership(t *testing.T, api *multica.Client) {
	s := supplementFixture(t, api, 78, "codex", true)
	comment := s.create(1, "owner only", 201)
	outsider, foreign := "b7000000-0000-4000-8000-000000000001", "b7000000-0000-4000-8000-000000000002"
	sql(t, fmt.Sprintf(`INSERT INTO "user"(id,name,email) VALUES('%s','Other owner','other-owner@example.invalid');
 INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','member');
 INSERT INTO workspace(id,name,slug) VALUES('%s','Foreign fixture','foreign-supplement');
 INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','owner');`, outsider, workspace, outsider, foreign, foreign, outsider))
	base, prefix := os.Getenv("MULTICA_TEST_URL"), "/api/daemon/tasks/"+s.task.ID+"/supplements"
	nativeAuthorityRequest(t, context.Background(), base, userToken(t, outsider), "POST", prefix+"/claim", `{}`, 404)
	nativeAuthorityRequest(t, context.Background(), base, userToken(t, outsider), "POST", prefix+"/"+comment+"/ack", `{"delivered":true}`, 404)
	s.receipt(comment, "pending||0")
	// The runtime's workspace must match the task issue's workspace.
	sql(t, fmt.Sprintf("UPDATE agent_runtime SET workspace_id='%s' WHERE id='%s';", foreign, s.task.RuntimeID))
	s.request("POST", prefix+"/claim", `{}`, 404)
	sql(t, fmt.Sprintf("UPDATE agent_runtime SET workspace_id='%s' WHERE id='%s';", workspace, s.task.RuntimeID))
	if s.claim() != comment {
		t.Fatal("unauthorized claims changed the supplement")
	}
	s.ack(comment, true, "", 200)
	s.finish()
}
