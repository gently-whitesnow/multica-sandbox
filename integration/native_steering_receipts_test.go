//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/examples/identity-mcp/faultproxy"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const (
	claimPath = `/supplements/claim$`
	ackPath   = `/supplements/[0-9a-f-]{36}/ack$`
	// steeringBackoff outlasts the client's first persisted retry delay (4 s).
	steeringBackoff = 4200 * time.Millisecond
)

type receiptView struct {
	Status     string `json:"task_status"`
	Delivering *struct {
		CommentID  string `json:"comment_id"`
		AuthorType string `json:"author_type"`
		AuthorName string `json:"author_name"`
		Content    string `json:"content"`
	} `json:"delivering"`
	Receipt *struct {
		CommentID string `json:"comment_id"`
		Status    string `json:"status"`
		Reason    string `json:"failure_reason"`
		Attempts  int    `json:"attempt_count"`
	} `json:"receipt"`
}

func (s *supplements) inspect(comment string) receiptView {
	s.t.Helper()
	path := "/api/daemon/tasks/" + s.task.ID + "/supplements"
	if comment != "" {
		path += "?comment_id=" + comment
	}
	var view receiptView
	if json.Unmarshal([]byte(s.request("GET", path, "", 200)), &view) != nil {
		s.t.Fatal("invalid receipt inspection")
	}
	return view
}

func steeringClient(t *testing.T, base, dir string) *multica.Client {
	t.Helper()
	client, err := multica.New(base, token(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.UseSupplements(dir); err != nil {
		t.Fatal(err)
	}
	return client
}

// arm adds a fault rule to a proxy; faults returns the hit count and currently
// held requests of the rules with that action and path pattern.
func arm(t *testing.T, proxy, action, match string, count int) {
	t.Helper()
	query := url.Values{"action": {action}, "match": {match}, "count": {fmt.Sprint(count)}}
	response, err := http.Post(proxy+faultproxy.Control+"?"+query.Encode(), "", nil)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("arm fault: %v", err)
	}
	_ = response.Body.Close()
}

func faults(t *testing.T, proxy, action, match string) (hits, held int) {
	t.Helper()
	response, err := http.Get(proxy + faultproxy.Control)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var rules []struct {
		Action, Match string
		Hits, Held    int
	}
	if json.NewDecoder(response.Body).Decode(&rules) != nil {
		t.Fatal("invalid fault state")
	}
	for _, rule := range rules {
		if rule.Action == action && rule.Match == match {
			hits, held = hits+rule.Hits, held+rule.Held
		}
	}
	return hits, held
}

func clearFaults(t *testing.T, proxy string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodDelete, proxy+faultproxy.Control, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

// nativeReceiptInspection pins the patched endpoint's contract on a real server.
func nativeReceiptInspection(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 84, "opencode", true, true)
	if view := s.inspect(""); view.Status != "running" || view.Delivering != nil || view.Receipt != nil {
		t.Fatalf("empty inspection: %+v", view)
	}
	first, second := s.create(1, "recover this exact input", 201), s.create(2, "keep this input pending", 201)
	loseSupplementResponse(t, "/api/daemon/tasks/"+s.task.ID+"/supplements/claim", `{"capabilities":["task-supplement-author-v1"]}`)
	view := s.inspect(second)
	if view.Delivering == nil || view.Delivering.CommentID != first || view.Delivering.Content != "recover this exact input" || view.Delivering.AuthorType != "member" || view.Delivering.AuthorName != "Contract fixture" {
		t.Fatalf("lost claim was not readable with its payload: %+v", view.Delivering)
	}
	if view.Receipt == nil || view.Receipt.CommentID != second || view.Receipt.Status != "pending" || view.Receipt.Attempts != 0 {
		t.Fatalf("pending receipt: %+v", view.Receipt)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM comment WHERE id='%s' AND '%s'::uuid = ANY(suppressed_agent_ids);", first, s.task.AgentID)); got != "1" {
		t.Fatal("claim did not preserve the member's current-run routing choice")
	}
	if got := s.inspect(first).Receipt; got == nil || got.Status != "delivering" || got.Attempts != 1 {
		t.Fatalf("claimed receipt: %+v", got)
	}
	path := "/api/daemon/tasks/" + s.task.ID + "/supplements"
	s.request("GET", path+"?comment_id=not-a-uuid", "", 400)
	if got := s.inspect("b9000000-0000-4000-8000-000000000001").Receipt; got == nil || got.Status != "none" {
		t.Fatalf("unknown receipt: %+v", got)
	}
	// Only the daemon that owns the runtime reads delivery state.
	outsider := "b9000000-0000-4000-8000-000000000002"
	sql(t, fmt.Sprintf(`INSERT INTO "user"(id,name,email) VALUES('%s','Receipt outsider','receipt-outsider@example.invalid');
 INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','member');`, outsider, workspace, outsider))
	nativeAuthorityRequest(t, context.Background(), os.Getenv("MULTICA_TEST_URL"), userToken(t, outsider), "GET", path, "", 404)
	// Multica itself refuses to delete a comment whose receipt is in flight.
	s.request("DELETE", "/api/comments/"+first, "", 409)
	s.receipt(first, "delivering||1")
	s.ack(first, true, "", 200)
	if view = s.inspect(first); view.Delivering != nil || view.Receipt.Status != "delivered" {
		t.Fatalf("acknowledged receipt still in delivery: %+v", view)
	}
	s.finish()
	if view = s.inspect(second); view.Status != "completed" || view.Receipt.Status != "failed" || view.Receipt.Reason != "turn_ended" {
		t.Fatalf("terminal inspection: %+v", view.Receipt)
	}
}

// nativeSteeringLargeHistory: neither a timeline over the native 2000-comment
// and 2 MiB windows nor more than 4096 receipts of one run affects delivery.
func nativeSteeringLargeHistory(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 85, "opencode", true, true)
	sql(t, fmt.Sprintf(`WITH history AS (
  INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,created_at,updated_at)
  SELECT '%[1]s','%[2]s','member','%[3]s',repeat('history ',150),'comment',now()-interval '1 hour'+n*interval '1 millisecond',now()
  FROM generate_series(1,5000) n RETURNING id)
 INSERT INTO task_supplement(task_id,workspace_id,issue_id,comment_id,author_id,client_request_id,status,attempt_count,delivered_at)
 SELECT '%[4]s','%[2]s','%[1]s',id,'%[3]s',gen_random_uuid(),'delivered',1,now() FROM history;`, s.task.IssueID, workspace, user, s.task.ID))
	request, _ := http.NewRequest("GET", os.Getenv("MULTICA_TEST_URL")+"/api/issues/"+s.task.IssueID+"/timeline", nil)
	request.Header.Set("Authorization", "Bearer "+token(t))
	request.Header.Set("X-Workspace-ID", workspace)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	buffer := make([]byte, 64<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		size += int64(n)
		if readErr != nil {
			break
		}
	}
	_ = response.Body.Close()
	if !strings.Contains(response.Header.Get("X-Timeline-Truncated"), "comment") || size <= 2<<20 {
		t.Fatalf("fixture history is not beyond the native timeline window: %d bytes, truncated=%q", size, response.Header.Get("X-Timeline-Truncated"))
	}
	client := steeringClient(t, os.Getenv("MULTICA_TEST_URL"), t.TempDir())
	if err = client.Start(context.Background(), s.task); err != nil || !client.SupplementsNegotiated(s.task) {
		t.Fatalf("large history negotiation: %v", err)
	}
	comments := []string{s.create(1, "first input after a large history", 201), s.create(2, "second input after a large history", 201)}
	for n, comment := range comments {
		progress, err := client.ProcessSupplement(context.Background(), s.task, "ses_largehistory", func(_ context.Context, _, text string) error {
			if !strings.Contains(text, []string{"first input", "second input"}[n]) {
				t.Error("large history reordered inputs")
			}
			return nil
		})
		if err != nil || !progress {
			t.Fatalf("large history blocked delivery %d: %v", n, err)
		}
		s.receipt(comment, "delivered||1")
	}
	s.finish()
}

// nativeSteeringVanishedReceipt removes a claimed receipt outside the comment
// API, which refuses that while it is in flight: cascades and repairs still can.
func nativeSteeringVanishedReceipt(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 86, "opencode", true, true, func(task string) {
		// Plan a real source thread before claim, as the controller fixture does.
		trigger := "b8600000-0000-4000-8000-000000000001"
		sql(t, fmt.Sprintf(`INSERT INTO comment(id,issue_id,workspace_id,author_type,author_id,content,type)
SELECT '%s',issue_id,'%s','member','%s','Original fixture input','comment' FROM agent_task_queue WHERE id='%s';
UPDATE agent_task_queue SET trigger_comment_id='%s' WHERE id='%s';`, trigger, workspace, user, task, trigger, task))
	})
	dir := t.TempDir()
	client := steeringClient(t, os.Getenv("MULTICA_TEST_URL"), dir)
	if err := client.Start(context.Background(), s.task); err != nil {
		t.Fatal(err)
	}
	vanish := func(comment string) {
		sql(t, fmt.Sprintf("DELETE FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", s.task.ID, comment))
	}
	consumed := s.create(1, "consumed before its receipt vanished", 201)
	progress, err := client.ProcessSupplement(context.Background(), s.task, "ses_vanished", func(context.Context, string, string) error {
		vanish(consumed)
		return nil
	})
	if err != nil || !progress || !client.SupplementsNegotiated(s.task) {
		t.Fatal("vanished receipt stopped the running attempt", err)
	}
	if data := steeringEvidence(t, dir, 1); !strings.Contains(data, consumed) || !strings.Contains(data, `"reason":"receipt_none"`) {
		t.Fatal("consumption evidence missing")
	}
	unconsumed := s.create(2, "claimed but never consumed", 201)
	if _, err = client.ProcessSupplement(context.Background(), s.task, "ses_vanished", func(context.Context, string, string) error {
		return errors.New("not consumed")
	}); err == nil {
		t.Fatal("fixture delivery failure hidden")
	}
	vanish(unconsumed)
	pending := s.create(4, "bound but never claimed", 201)
	vanish(pending)
	later := s.create(3, "normal input after both", 201)
	texts := []string{}
	for range 3 {
		if _, err = client.ProcessSupplement(context.Background(), s.task, "ses_vanished", func(_ context.Context, _, text string) error {
			texts = append(texts, text)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "normal input after both") {
		t.Fatalf("later input blocked or vanished input injected: %d", len(texts))
	}
	s.receipt(later, "delivered||1")
	steeringEvidence(t, dir, 1)
	s.finish()
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE issue_id='%s';", s.task.IssueID)); got != "1" {
		t.Fatalf("missing consumed/unconsumed receipts created a follow-up: %s", got)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM task_supplement WHERE comment_id='%s';", unconsumed)); got != "0" {
		t.Fatal("vanished unconsumed input unexpectedly has a visible receipt")
	}
}

// nativeSteeringLostResponses loses real HTTP responses of committed native
// requests and holds an acknowledgement across completion and a client restart.
func nativeSteeringLostResponses(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 87, "opencode", true, true)
	upstream, _ := url.Parse(os.Getenv("MULTICA_TEST_URL"))
	proxy := httptest.NewServer(faultproxy.Handler(upstream))
	defer proxy.Close()
	dir := t.TempDir()
	client := steeringClient(t, proxy.URL, dir)
	if err := client.Start(context.Background(), s.task); err != nil || !client.SupplementsNegotiated(s.task) {
		t.Fatal("negotiation through the fault proxy", err)
	}
	first, second := s.create(1, "first ambiguous input", 201), s.create(2, "second ordered input", 201)
	texts := []string{}
	deliver := func(_ context.Context, _, text string) error { texts = append(texts, text); return nil }
	arm(t, proxy.URL, faultproxy.Lose, claimPath, 1)
	if _, err := client.ProcessSupplement(context.Background(), s.task, "ses_lostresponses", deliver); err == nil || len(texts) != 0 {
		t.Fatal("lost claim response hidden or delivered blindly")
	}
	s.receipt(first, "delivering||1")
	time.Sleep(steeringBackoff)
	arm(t, proxy.URL, faultproxy.Lose, ackPath, 1)
	if _, err := client.ProcessSupplement(context.Background(), s.task, "ses_lostresponses", deliver); err == nil || len(texts) != 1 {
		t.Fatal("lost acknowledgement response hidden", err)
	}
	s.receipt(first, "delivered||1")
	time.Sleep(steeringBackoff)
	for range 2 {
		if _, err := client.ProcessSupplement(context.Background(), s.task, "ses_lostresponses", deliver); err != nil {
			t.Fatal(err)
		}
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "first ambiguous") || !strings.Contains(texts[1], "second ordered") {
		t.Fatalf("ambiguous outcomes duplicated or reordered input: %d", len(texts))
	}
	s.receipt(first, "delivered||1")
	s.receipt(second, "delivered||1")
	// Consumption is durable, the acknowledgement never reaches Multica, the run
	// completes and the client is replaced: only the receipt is replayed.
	third := s.create(3, "consumed before a held acknowledgement", 201)
	arm(t, proxy.URL, faultproxy.Hold, ackPath, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = client.ProcessSupplement(ctx, s.task, "ses_lostresponses", deliver)
	}()
	eventually(t, "acknowledgement held before Multica", func() bool { _, held := faults(t, proxy.URL, faultproxy.Hold, ackPath); return held == 1 })
	s.receipt(third, "delivering||1")
	s.finish()
	s.receipt(third, "failed|turn_ended|1")
	cancel()
	<-done
	clearFaults(t, proxy.URL)
	time.Sleep(steeringBackoff)
	restarted := steeringClient(t, proxy.URL, dir)
	if n, err := restarted.ReplaySupplementAcks(context.Background()); n != 1 || err != nil {
		t.Fatalf("late acknowledgement replay=%d: %v", n, err)
	}
	s.receipt(third, "delivered||1")
	if len(texts) != 3 || len(steeringJournals(t, dir)) != 0 {
		t.Fatal("restart reinjected input or kept a terminal journal")
	}
	steeringEvidence(t, dir, 0)
}

// nativeSteeringCancelRace: a claim that was never consumed is not acknowledged
// when the run is cancelled; Multica settles it as turn_ended.
func nativeSteeringCancelRace(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 88, "opencode", true, true)
	dir := t.TempDir()
	client := steeringClient(t, os.Getenv("MULTICA_TEST_URL"), dir)
	if err := client.Start(context.Background(), s.task); err != nil {
		t.Fatal(err)
	}
	comment := s.create(1, "claimed while the run is cancelled", 201)
	if _, err := client.ProcessSupplement(context.Background(), s.task, "ses_cancelrace", func(context.Context, string, string) error {
		s.request("POST", "/api/tasks/"+s.task.ID+"/cancel", `{}`, 200)
		return errors.New("turn ended before consumption")
	}); err == nil {
		t.Fatal("unconsumed input reported as delivered")
	}
	if err := api.CancelAck(context.Background(), s.task.ID); err != nil {
		t.Fatal(err)
	}
	forbidden := func(context.Context, string, string) error { t.Error("cancelled input injected"); return nil }
	if _, err := client.ProcessSupplement(context.Background(), s.task, "ses_cancelrace", forbidden); !errors.Is(err, multica.ErrSupplementEnded) || client.SupplementsNegotiated(s.task) {
		t.Fatal("cancelled run kept accepting input", err)
	}
	if n, err := client.ReplaySupplementAcks(context.Background()); n != 0 || err != nil || len(steeringJournals(t, dir)) != 0 {
		t.Fatal("cancelled claim was acknowledged or its journal kept", err)
	}
	s.receipt(comment, "failed|turn_ended|1")
	steeringEvidence(t, dir, 0)
}
