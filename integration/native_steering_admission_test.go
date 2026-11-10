//go:build upstream

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func nativeSteeringAdmission(t *testing.T, api *multica.Client) {
	if os.Getenv("VERIFY_STEERING_NATIVE_PATCH") != "1" {
		t.Skip("requires the opt-in disposable native admission patch")
	}
	s := supplementAdmissionFixture(t, api, 79, "opencode", true, true)
	if got := sql(t, fmt.Sprintf("SELECT provider FROM agent_runtime WHERE id='%s';", s.task.RuntimeID)); got != "opencode" {
		t.Fatalf("native admission fixture impersonates provider %q", got)
	}
	direct := s.create(1, "direct native input", 201)
	response := s.request("POST", "/api/issues/"+s.task.IssueID+"/comments", fmt.Sprintf(`{"content":"[@Fixture](mention://agent/%s) composer input","steer_task_ids":[%q]}`, s.task.AgentID, s.task.ID), 201)
	var comment struct {
		ID       string
		Outcomes []struct{ Status string } `json:"trigger_outcomes"`
	}
	if json.Unmarshal([]byte(response), &comment) != nil || len(comment.Outcomes) != 1 || comment.Outcomes[0].Status != "steered" {
		t.Fatalf("native composer did not bind to the active OpenCode run: %s", response)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id='%s';", comment.ID)); got != "0" {
		t.Fatal("steered comment created a follow-up task")
	}
	for _, id := range []string{direct, comment.ID} {
		if got := s.claim(); got != id {
			t.Fatalf("native ordered claim=%s, want %s", got, id)
		}
		s.ack(id, true, "", 200)
	}
	s.finish()
	missing := supplementAdmissionFixture(t, api, 80, "opencode", false, false)
	missing.create(1, "no capability", 412)
	missing.finish()
	t.Run("client-journal", func(t *testing.T) { nativeSupplementJournal(t, api) })
	t.Run("client-terminal-ack-quarantine", func(t *testing.T) { nativeSupplementAckQuarantine(t, api) })
	t.Run("receipt-inspection", func(t *testing.T) { supplementInspection(t, api) })
	t.Log("fixture-only SQL admission enables native OpenCode negotiation, direct creation and composer binding; no agent delivery is asserted")
}

func nativeSupplementJournal(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 82, "opencode", true, true)
	client, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.UseSupplements(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err = client.Start(context.Background(), s.task); err != nil || !client.SupplementsNegotiated(s.task) {
		t.Fatalf("native journal negotiation: %v", err)
	}
	comments := []string{s.create(1, "first journal fixture input", 201), s.create(2, "second journal fixture input", 201)}
	nativeJournalBinding(t, s)
	ids := []string{}
	for n, comment := range comments {
		progress, err := client.ProcessSupplement(context.Background(), s.task, "ses_nativejournal", func(ctx context.Context, id, text string) error {
			if !strings.Contains(text, []string{"first journal", "second journal"}[n]) {
				t.Fatal("native claim order changed")
			}
			status, err := api.Status(ctx, s.task.ID)
			if err != nil || status != "running" {
				t.Fatal("fixture callback did not run inside the original native run")
			}
			ids = append(ids, id)
			return nil // Deterministic consumption callback, not an OpenCode model.
		})
		if err != nil || !progress {
			t.Fatalf("real native journal delivery: %v", err)
		}
		s.receipt(comment, "delivered||1")
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatal("native journal lost per-message identity")
	}
	if progress, err := client.ProcessSupplement(context.Background(), s.task, "ses_nativejournal", func(context.Context, string, string) error {
		t.Fatal("empty native queue reinjected input")
		return nil
	}); err != nil || progress {
		t.Fatalf("empty native journal: %v", err)
	}
	s.finish()
	if _, err = client.ReplaySupplementAcks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.SupplementsNegotiated(s.task) {
		t.Fatal("externally completed task retained steering capability")
	}
}

func nativeJournalBinding(t *testing.T, s *supplements) {
	raw, err := s.task.TaskToken()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := supplementRead(t, raw, "/api/issues/"+s.task.IssueID+"/task-runs")
	var tasks []multica.Task
	if json.Unmarshal(body, &tasks) != nil || len(tasks) != 1 {
		t.Fatal("native journal fixture did not return its exact task")
	}
	got, want := tasks[0], s.task
	fields := map[string][2]string{"task": {got.ID, want.ID}, "runtime": {got.RuntimeID, want.RuntimeID}, "workspace": {got.WorkspaceID, want.WorkspaceID}, "agent": {got.AgentID, want.AgentID}, "issue": {got.IssueID, want.IssueID}}
	for name, pair := range fields {
		if pair[0] != pair[1] {
			t.Fatalf("native journal binding %s differs: got=%q want=%q", name, pair[0], pair[1])
		}
	}
	claimed, err := time.Parse(time.RFC3339Nano, want.DispatchedAt)
	if err != nil || got.DispatchedAt != claimed.Truncate(time.Second).Format(time.RFC3339) {
		t.Fatal("native public dispatch display contract changed")
	}
}

type supplementRunView struct {
	ID         string
	Capability string   `json:"supplement_capability"`
	Comments   []string `json:"supplement_comment_ids"`
}

type supplementTimelineView struct {
	ID          string
	Content     string
	ActorType   string `json:"actor_type"`
	Supplements []struct {
		TaskID string `json:"task_id"`
		Status string
	} `json:"supplements"`
}

func supplementInspection(t *testing.T, api *multica.Client) {
	s := supplementFixture(t, api, 81, "codex", true)
	first, second := s.create(1, "recover this exact input", 201), s.create(2, "keep this input pending", 201)
	loseSupplementResponse(t, "/api/daemon/tasks/"+s.task.ID+"/supplements/claim", `{}`)
	pat := "mul_disposable_supplement_inspection_fixture"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(pat)))
	sql(t, fmt.Sprintf("INSERT INTO personal_access_token(user_id,name,token_hash,token_prefix) VALUES('%s','Supplement inspection','%s','mul_fixture');", user, hash))
	raw, err := s.task.TaskToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{token(t), pat, raw} {
		inspectSupplementReceipts(t, s, credential, first, second)
	}
	supplementInspectionAuth(t, s, first, second)
	// The native comments endpoint does not hydrate delivery receipts.
	comments := s.request("GET", "/api/issues/"+s.task.IssueID+"/comments", "", 200)
	if strings.Contains(comments, `"supplements"`) || strings.Contains(comments, `"supplement_status"`) {
		t.Fatal("comments inspection contract changed; revisit receipt recovery")
	}
	t.Run("metadata-absent", func(t *testing.T) { supplementMissingMetadata(t, s, pat) })
	t.Run("timeline-truncated", func(t *testing.T) { supplementTruncatedTimeline(t, s, pat, first) })
	s.finish()
}

func supplementInspectionAuth(t *testing.T, s *supplements, first, second string) {
	other := "b8000000-0000-4000-8000-000000000001"
	daemonToken := "mdt_disposable_supplement_inspection"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(daemonToken)))
	sql(t, fmt.Sprintf(`INSERT INTO "user"(id,name,email) VALUES('%s','Inspection member','inspection-member@example.invalid');
 INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','member');
 INSERT INTO daemon_token(token_hash,workspace_id,daemon_id,expires_at,owner_id)
 VALUES('%s','%s','d0000000-0000-4000-8000-000000000081',now()+interval '1 hour','%s');`, other, workspace, other, hash, workspace, user))
	// Inspection is a workspace-member read, not runtime-owner authorization.
	inspectSupplementReceipts(t, s, userToken(t, other), first, second)
	base := os.Getenv("MULTICA_TEST_URL")
	nativeAuthorityRequest(t, context.Background(), base, userToken(t, other), "POST", "/api/daemon/tasks/"+s.task.ID+"/supplements/claim", `{}`, 404)
	// A runtime-only daemon credential can poll its exact task, but normal
	// issue APIs use user/task-token Auth rather than DaemonAuth.
	nativeAuthorityRequest(t, context.Background(), base, daemonToken, "GET", "/api/daemon/tasks/"+s.task.ID+"/status", "", 200)
	nativeAuthorityRequest(t, context.Background(), base, daemonToken, "GET", "/api/issues/"+s.task.IssueID+"/task-runs", "", 401)
	s.receipt(first, "delivering||1")
	s.receipt(second, "pending||0")
}

func inspectSupplementReceipts(t *testing.T, s *supplements, credential, first, second string) {
	t.Helper()
	prefix := "/api/issues/" + s.task.IssueID
	runs, _ := supplementRead(t, credential, prefix+"/task-runs")
	var tasks []supplementRunView
	if json.Unmarshal(runs, &tasks) != nil || len(tasks) != 1 || tasks[0].ID != s.task.ID || tasks[0].Capability != supplementCapability || !slices.Equal(tasks[0].Comments, []string{first, second}) {
		t.Fatal("task-runs did not expose the exact task's complete supplement IDs")
	}
	timeline, headers := supplementRead(t, credential, prefix+"/timeline")
	if strings.Contains(headers.Get("X-Timeline-Truncated"), "comment") {
		t.Fatal("small fixture timeline unexpectedly truncated")
	}
	var entries []supplementTimelineView
	if json.Unmarshal(timeline, &entries) != nil {
		t.Fatal("invalid native timeline")
	}
	want := map[string]string{first: "delivering", second: "pending"}
	for _, entry := range entries {
		if _, ok := want[entry.ID]; !ok {
			continue
		}
		if entry.ActorType != "member" || len(entry.Supplements) != 1 || entry.Supplements[0].TaskID != s.task.ID || entry.Supplements[0].Status != want[entry.ID] || entry.Content == "" {
			t.Fatalf("incomplete exact-task receipt inspection for %s", entry.ID)
		}
		delete(want, entry.ID)
	}
	if len(want) != 0 {
		t.Fatal("native timeline omitted a known supplement")
	}
}

func supplementRead(t *testing.T, credential, path string) ([]byte, http.Header) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), "GET", os.Getenv("MULTICA_TEST_URL")+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Workspace-ID", workspace)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("supplement inspection HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	return body, response.Header
}

func supplementMissingMetadata(t *testing.T, s *supplements, credential string) {
	// Missing optional metadata must not be interpreted as an empty supported
	// queue. The server also omits it when its hydration query fails.
	sql(t, fmt.Sprintf("DELETE FROM task_supplement_capability WHERE task_id='%s';", s.task.ID))
	body, _ := supplementRead(t, credential, "/api/issues/"+s.task.IssueID+"/task-runs")
	var tasks []supplementRunView
	if json.Unmarshal(body, &tasks) != nil || len(tasks) != 1 || tasks[0].Capability != "" || len(tasks[0].Comments) != 0 {
		t.Fatal("missing native metadata did not omit capability and supplement IDs")
	}
	sql(t, fmt.Sprintf("INSERT INTO task_supplement_capability(task_id,workspace_id,issue_id,capability) VALUES('%s','%s','%s','%s');", s.task.ID, workspace, s.task.IssueID, supplementCapability))
}

func supplementTruncatedTimeline(t *testing.T, s *supplements, credential, first string) {
	sql(t, fmt.Sprintf(`INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,created_at,updated_at)
 SELECT '%s','%s','member','%s','Timeline filler','comment',now()+n*interval '1 second',now()
 FROM generate_series(1,2001) n;`, s.task.IssueID, workspace, user))
	body, headers := supplementRead(t, credential, "/api/issues/"+s.task.IssueID+"/timeline")
	if !strings.Contains(headers.Get("X-Timeline-Truncated"), "comment") {
		t.Fatal("native timeline did not signal the comment hard cap")
	}
	var entries []supplementTimelineView
	if json.Unmarshal(body, &entries) != nil {
		t.Fatal("invalid truncated native timeline")
	}
	for _, entry := range entries {
		if entry.ID == first {
			t.Fatal("old fixture receipt unexpectedly survived the newest-comment cap")
		}
	}
	runs, _ := supplementRead(t, credential, "/api/issues/"+s.task.IssueID+"/task-runs")
	var tasks []supplementRunView
	if json.Unmarshal(runs, &tasks) != nil || len(tasks) != 1 || !slices.Contains(tasks[0].Comments, first) {
		t.Fatal("task-runs lost the ID of a receipt omitted by timeline truncation")
	}
}
