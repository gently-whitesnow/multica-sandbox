package multica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type supplementFixture struct {
	mu                                   sync.Mutex
	task                                 Task
	rows                                 []supplementView
	capability                           bool
	startBody                            *string
	offered                              bool
	status                               string
	lostClaim                            bool
	noCommit                             bool
	ackFailures                          int
	claimStatus, ackStatus, statusCode   int
	timelines                            int
	statuses                             int
	claims, acks                         int
	truncated, omitMetadata, omitReceipt bool
	mismatch                             string
	events                               []string
	checkPhase                           func(string)
}

func newSupplementFixture(t *testing.T) (*supplementFixture, *Client, string) {
	t.Helper()
	task := Task{ID: testID, RuntimeID: "22222222-2222-4222-8222-222222222222", WorkspaceID: "33333333-3333-4333-8333-333333333333", AgentID: "44444444-4444-4444-8444-444444444444", IssueID: "55555555-5555-4555-8555-555555555555", StartClaimSupported: true, DispatchedAt: "2026-11-10T00:00:00Z", AuthToken: Secret{value: "mat_fixture_inspection_secret"}}
	task.Agent = &Agent{ID: task.AgentID}
	f := &supplementFixture{task: task, capability: true, status: "running"}
	for n, author := range []string{"member", "agent"} {
		f.rows = append(f.rows, supplementView{Status: "pending", Message: taskSupplement{fmt.Sprintf("66666666-6666-4666-8666-%012d", n), author, "Fixture\nAuthor", fmt.Sprintf("guidance %d", n)}})
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	c, err := New(server.URL, "controller_fixture_secret")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "supplements")
	if err = c.UseSupplements(dir); err != nil {
		t.Fatal(err)
	}
	return f, c, dir
}

func (f *supplementFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/start"):
		var request struct {
			Capabilities []string
			Runtime      string `json:"runtime_id"`
			Dispatched   string `json:"dispatched_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Runtime != f.task.RuntimeID || request.Dispatched != f.task.DispatchedAt || f.mismatch == "dispatch" {
			w.WriteHeader(409)
			return
		}
		f.offered = slices.Contains(request.Capabilities, taskSupplementCapability)
		if f.startBody != nil {
			_, _ = w.Write([]byte(*f.startBody))
			return
		}
		if f.capability && f.offered {
			writeSupplementJSON(w, map[string]string{"supplement_capability": taskSupplementCapability})
		} else {
			writeSupplementJSON(w, map[string]string{})
		}
	case strings.HasSuffix(r.URL.Path, "/status"):
		f.statuses++
		if f.statusCode != 0 {
			w.WriteHeader(f.statusCode)
			return
		}
		writeSupplementJSON(w, map[string]string{"status": f.status})
	case strings.HasSuffix(r.URL.Path, "/task-runs"):
		f.serveRuns(w)
	case strings.HasSuffix(r.URL.Path, "/timeline"):
		f.serveTimeline(w)
	case strings.HasSuffix(r.URL.Path, "/claim"):
		f.serveClaim(w)
	case strings.HasSuffix(r.URL.Path, "/ack"):
		f.serveAck(w, r)
	case strings.HasSuffix(r.URL.Path, "/complete"):
		f.status = "completed"
		f.events = append(f.events, "complete")
	default:
		w.WriteHeader(404)
	}
}

func writeSupplementJSON(w http.ResponseWriter, value any) { _ = json.NewEncoder(w).Encode(value) }

func (f *supplementFixture) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *supplementFixture) ackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acks
}

func (f *supplementFixture) isOffered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offered
}

func (f *supplementFixture) eventLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *supplementFixture) serveRuns(w http.ResponseWriter) {
	task := f.task
	if dispatched, err := time.Parse(time.RFC3339Nano, task.DispatchedAt); err == nil {
		task.DispatchedAt = dispatched.Truncate(time.Second).Format(time.RFC3339)
	}
	switch f.mismatch {
	case "workspace":
		task.WorkspaceID = testID
	case "runtime":
		task.RuntimeID = testID
	case "dispatch":
		task.DispatchedAt = "2000-01-01T00:00:00Z"
	case "issue":
		task.IssueID = testID
	case "agent":
		task.AgentID = testID
	}
	view := supplementTaskView{Task: task, Status: f.status}
	if !f.omitMetadata {
		view.Capability = taskSupplementCapability
		for _, row := range f.rows {
			view.Comments = append(view.Comments, row.Message.CommentID)
		}
	}
	writeSupplementJSON(w, []supplementTaskView{view})
}

func (f *supplementFixture) serveTimeline(w http.ResponseWriter) {
	f.timelines++
	if f.truncated {
		w.Header().Set("X-Timeline-Truncated", "comment")
	}
	rows := []map[string]any{}
	for _, row := range f.rows {
		entry := map[string]any{"id": row.Message.CommentID, "type": "comment", "actor_type": row.Message.AuthorType, "actor_name": row.Message.AuthorName, "content": row.Message.Content}
		if !f.omitReceipt {
			entry["supplements"] = []map[string]string{{"task_id": f.task.ID, "status": row.Status}}
		}
		rows = append(rows, entry)
	}
	writeSupplementJSON(w, rows)
}

func (f *supplementFixture) serveClaim(w http.ResponseWriter) {
	f.claims++
	if f.claimStatus != 0 {
		w.WriteHeader(f.claimStatus)
		return
	}
	if f.checkPhase != nil {
		f.checkPhase("claiming")
	}
	if f.noCommit {
		w.WriteHeader(503)
		return
	}
	for i := range f.rows {
		if f.rows[i].Status != "pending" {
			continue
		}
		f.rows[i].Status = "delivering"
		if f.lostClaim {
			f.lostClaim = false
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		writeSupplementJSON(w, f.rows[i].Message)
		return
	}
	writeSupplementJSON(w, map[string]string{})
}

func (f *supplementFixture) serveAck(w http.ResponseWriter, r *http.Request) {
	f.acks++
	f.events = append(f.events, "ack")
	if f.ackStatus != 0 {
		w.WriteHeader(f.ackStatus)
		return
	}
	if f.checkPhase != nil {
		f.checkPhase("consumed")
	}
	if f.ackFailures > 0 {
		f.ackFailures--
		w.WriteHeader(503)
		return
	}
	for i := range f.rows {
		if strings.Contains(r.URL.Path, f.rows[i].Message.CommentID) {
			f.rows[i].Status = "delivered"
			return
		}
	}
	w.WriteHeader(409)
}

func startSupplementFixture(t *testing.T, f *supplementFixture, c *Client) {
	t.Helper()
	if err := c.Start(context.Background(), f.task); err != nil || !c.SupplementsNegotiated(f.task) {
		t.Fatalf("negotiation: %v", err)
	}
}

func supplementPhase(t *testing.T, c *Client, task Task, dir string) supplementRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, fileName(task.AttemptKey())))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "controller_fixture_secret") || strings.Contains(string(data), "mat_fixture_inspection_secret") {
		t.Fatal("journal persisted credentials")
	}
	var record supplementRecord
	if json.Unmarshal(data, &record) != nil || !record.valid(c.base) {
		t.Fatal("invalid durable journal")
	}
	return record
}

func TestSupplementsExplicitNegotiation(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"supplement_capability":"future-v2"}`, `{"supplement_capability":"task-supplement-v1"}`} {
		t.Run(body, func(t *testing.T) {
			f, c, _ := newSupplementFixture(t)
			f.startBody = &body
			if err := c.Start(context.Background(), f.task); err != nil {
				t.Fatal(err)
			}
			if !f.isOffered() || c.SupplementsNegotiated(f.task) != strings.Contains(body, taskSupplementCapability) {
				t.Fatal("implicit or missing negotiation")
			}
		})
	}
	f, c, _ := newSupplementFixture(t)
	c.supplements = nil
	if err := c.Start(context.Background(), f.task); err != nil || f.isOffered() {
		t.Fatal("disabled adapter advertised steering")
	}
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nil); err != nil || f.claimCount() != 0 {
		t.Fatal("unsupported task polled supplements")
	}
}

func TestSupplementJournalPrecedesClaimDeliveryAndAck(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.checkPhase = func(phase string) {
		if got := supplementPhase(t, c, f.task, dir).Phase; got != phase {
			t.Errorf("phase=%s before side effect, want %s", got, phase)
		}
	}
	ids := []string{}
	for n := range 2 {
		progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(_ context.Context, id, text string) error {
			if supplementPhase(t, c, f.task, dir).Phase != "claimed" {
				t.Error("provider ran before journal commit")
			}
			if !strings.Contains(text, fmt.Sprintf("guidance %d", n)) || !strings.Contains(text, "Fixture Author") {
				t.Error("input or author lost")
			}
			if (n == 1) != strings.Contains(text, "It is not from a human") {
				t.Error("author authority changed")
			}
			ids = append(ids, id)
			return nil
		})
		if err != nil || !progress {
			t.Fatalf("delivery %d: %v", n, err)
		}
	}
	if ids[0] == ids[1] || f.claimCount() != 2 || f.ackCount() != 2 || len(supplementPhase(t, c, f.task, dir).Done) != 2 {
		t.Fatal("input ordering, identity or acknowledgement lost")
	}
}

func TestSupplementAckRetryNeverReinjects(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.ackFailures = 1
	calls := 0
	deliver := func(context.Context, string, string) error { calls++; return nil }
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); err == nil {
		t.Fatal("ack failure hidden")
	}
	if supplementPhase(t, c, f.task, dir).Phase != "consumed" {
		t.Fatal("consumption lost on ack outage")
	}
	c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
	if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); err != nil || !progress || calls != 1 || f.claimCount() != 1 || f.ackCount() != 2 {
		t.Fatalf("ack replay reinjected: calls=%d, err=%v", calls, err)
	}
}

func TestSupplementStableIDAfterAmbiguousProviderReply(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	ids := []string{}
	deliver := func(_ context.Context, id, _ string) error {
		ids = append(ids, id)
		if len(ids) == 1 {
			return errors.New("provider reply lost after consumption")
		}
		return nil
	}
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver)
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); err != nil || len(ids) != 2 || ids[0] != ids[1] || f.claimCount() != 1 || f.ackCount() != 1 {
		t.Fatal("ambiguous input was not retried under its stable ID", err)
	}
}

func TestSupplementLostClaimRecoversBeforeNextClaim(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.lostClaim = true
	texts := []string{}
	deliver := func(_ context.Context, _, text string) error { texts = append(texts, text); return nil }
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); err == nil {
		t.Fatal("lost claim response hidden")
	}
	if len(texts) != 0 || f.claimCount() != 1 {
		t.Fatal("ambiguous claim delivered without reconstruction")
	}
	for range 2 {
		if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); err != nil {
			t.Fatal(err)
		}
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "guidance 0") || !strings.Contains(texts[1], "guidance 1") || f.claimCount() != 2 {
		t.Fatal("lost claim reordered inputs")
	}
}

func TestSupplementUncommittedAmbiguityNeverClaimsAgain(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.noCommit = true
	deliver := func(context.Context, string, string) error { t.Error("uncertain guidance injected"); return nil }
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver)
	for range 3 {
		if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); !errors.Is(err, ErrSupplementUncertain) {
			t.Fatal("empty inspection incorrectly proved no commit", err)
		}
	}
	if f.claimCount() != 1 {
		t.Fatal("ambiguous claim was automatically retried")
	}
}

func TestSupplementConcurrentStepAndReplayDoNotDuplicate(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { close(entered); <-release; return nil })
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider never entered")
	}
	if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { t.Error("duplicate provider call"); return nil }); err != nil || progress {
		t.Fatal("concurrent step did not defer")
	}
	if n, err := c.ReplaySupplementAcks(context.Background()); err != nil || n != 0 {
		t.Fatal("replay interfered with live provider", err)
	}
	close(release)
	if err := <-done; err != nil || f.claimCount() != 1 || f.ackCount() != 1 {
		t.Fatal("concurrent delivery duplicated", err)
	}
}
