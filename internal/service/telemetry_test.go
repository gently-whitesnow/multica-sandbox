package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func TestFinishedEventSummarizesAttempts(t *testing.T) {
	c, log, m := observed(t, false)
	claimed := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	task := multica.Task{ID: "task-1", WorkspaceID: "ws-1", AgentID: "agent-1", IssueID: "issue-1", TriggerCommentContent: "prompt text"}
	c.finished(controller.Finish{Task: task, Claimed: claimed, Started: claimed.Add(4800 * time.Millisecond), Ended: claimed.Add(15 * time.Second),
		Result: execution.Result{Output: "agent output", SessionID: "ses_1", Telemetry: execution.Telemetry{Model: "inference-codex/gpt-demo", Effort: "high", Continuity: "resumed",
			InputTokens: 1200, OutputTokens: 80, CachedTokens: 900, PeakMemory: 512 << 20}}})
	task.IssueID, task.ChatSessionID = "", "chat-1"
	c.finished(controller.Finish{Task: task, Claimed: claimed, Ended: claimed.Add(time.Second), Deferred: true, Failure: &execution.RejectedError{Err: errSecret}})
	c.finished(controller.Finish{Task: task, Claimed: claimed, Started: claimed, Ended: claimed, Result: execution.Result{Telemetry: execution.Telemetry{Continuity: "fresh"}},
		Failure: &execution.AgentFailure{Status: 429}})

	events := lines(t, log)
	if len(events) != 3 {
		t.Fatalf("%d events: %s", len(events), log)
	}
	want := []map[string]any{
		{"level": "INFO", "msg": "attempt finished", "task": "task-1", "workspace": "ws-1", "agent": "agent-1", "kind": "issue", "status": "success",
			"prepare_seconds": 4.8, "run_seconds": 10.2, "continuity": "resumed", "total_seconds": 15.0, "model": "inference-codex/gpt-demo", "effort": "high",
			"tokens_in": 1200.0, "tokens_out": 80.0, "tokens_cached": 900.0, "peak_memory_bytes": float64(512 << 20), "session": "ses_1"},
		{"level": "WARN", "msg": "attempt finished", "kind": "chat", "status": "failed", "reason": "rejected", "failure_reason": "environment_prepare_failed",
			"total_seconds": 1.0, "report_deferred": true, "error": "Sandbox rejected the task before OpenCode started"},
		{"level": "WARN", "kind": "chat", "status": "failed", "reason": "inference", "status_code": 429.0, "error": "Agent inference request failed (HTTP 429)"},
	}
	for i, fields := range want {
		for key, value := range fields {
			if events[i][key] != value {
				t.Errorf("event %d %s: %v, want %v", i, key, events[i][key], value)
			}
		}
	}
	text := exposition(m)
	for _, line := range []string{
		`sandbox_attempt_duration_seconds_count{kind="issue",status="success"} 1`,
		`sandbox_attempt_duration_seconds_count{kind="chat",reason="rejected",status="failed"} 1`,
		`sandbox_attempt_duration_seconds_count{kind="chat",reason="inference",status="failed",status_code="429"} 1`,
		`sandbox_attempt_prepare_duration_seconds_count{continuity="resumed",kind="issue"} 1`,
		`sandbox_attempt_prepare_duration_seconds_count{continuity="fresh",kind="chat"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("exposition lacks %s", line)
		}
	}
	for _, leak := range []string{"secret native detail", "prompt text", "agent output"} {
		if strings.Contains(log.String(), leak) || strings.Contains(text, leak) {
			t.Errorf("%q leaked", leak)
		}
	}
}

func TestOutcomeClassifiesFailures(t *testing.T) {
	for _, test := range []struct {
		cause          error
		status, reason string
		code           int
	}{
		{nil, "success", "", 0},
		{&execution.TimeoutError{After: time.Minute}, "timeout", "", 0},
		{&execution.IdleError{After: time.Minute}, "failed", "idle", 0},
		{&execution.OOMError{}, "failed", "oom", 0},
		{&execution.MemoryStallError{}, "failed", "memory_stall", 0},
		{&execution.HostFailure{}, "failed", "host", 0},
		{&execution.RejectedError{Err: errors.New("image")}, "failed", "rejected", 0},
		{execution.Transient(errors.New("network")), "failed", "rejected_transient", 0},
		{&execution.AgentFailure{Status: 403}, "failed", "inference", 403},
		{&execution.AgentFailure{Message: "opencode cancelled the turn"}, "failed", "agent", 0},
		{errors.New("identity delivery"), "failed", "internal", 0},
	} {
		if status, reason, code := outcome(test.cause); status != test.status || reason != test.reason || code != test.code {
			t.Errorf("%v: %s %s %d", test.cause, status, reason, code)
		}
	}
}

// Lifecycle events are logged; those outside the duration histogram are counted.
func TestAttemptEvents(t *testing.T) {
	c, log, m := observed(t, false)
	c.attemptEvent("started", "task-1")
	c.attemptEvent("cancelled", "task-1")
	c.attemptEvent("rejected (Sandbox rejected the task before OpenCode started)", "task-2")
	events := lines(t, log)
	if events[0]["msg"] != "attempt started" || events[2]["msg"] != "attempt rejected" || events[2]["error"] != "Sandbox rejected the task before OpenCode started" {
		t.Fatalf("events: %v", events)
	}
	text := exposition(m)
	if !strings.Contains(text, `sandbox_attempt_events_total{event="cancelled"} 1`) || strings.Contains(text, `event="started"`) || strings.Contains(text, `event="rejected"`) {
		t.Fatalf("event counts:\n%s", text)
	}
}

// Relay refusals are recorded and logged at warning level without debug: task and
// workspace, a redacted path, never the query or headers.
func TestRelayRecordLogsRefusals(t *testing.T) {
	c, log, m := observed(t, false)
	refusals := newRefusalLog(c.logger())
	record := c.relayRecord("inference", refusals)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=secret", nil)
	req.Header.Set("Authorization", "Bearer opaque-secret")
	attempt := "10000000-0000-4000-8000-000000000001:runtime:20000000-0000-4000-8000-000000000002:2026-01-02T03:04:05Z"
	record(req, attempt, 200, nil, time.Second)
	record(req, "", 0, relay.ErrPolicy, time.Millisecond)
	record(req, attempt, 429, nil, time.Second)
	record(req, attempt, 0, relay.ErrBusy, time.Millisecond)
	events := lines(t, log)
	if len(events) != 3 || events[0]["level"] != "WARN" || events[0]["msg"] != "relay request refused" || events[0]["error_type"] != "policy-denied" || events[0]["task"] != nil {
		t.Fatalf("events: %v", events)
	}
	if e := events[1]; e["msg"] != "relay upstream error" || e["status"] != 429.0 || e["path"] != "/v1/chat/completions" || e["task"] != "20000000-0000-4000-8000-000000000002" || e["workspace"] != "10000000-0000-4000-8000-000000000001" || e["error_type"] != nil {
		t.Fatalf("upstream event: %v", e)
	}
	if events[2]["error_type"] != "too-many-requests" || events[2]["status"] != nil {
		t.Fatalf("busy event: %v", events[2])
	}
	if strings.Contains(log.String(), "secret") {
		t.Fatal("query or credential logged")
	}
	text := exposition(m)
	for _, line := range []string{
		`sandbox_relay_request_duration_seconds_count{relay="inference",status_code="429"} 1`,
		`sandbox_relay_request_duration_seconds_count{error_type="policy-denied",relay="inference"} 1`,
		`sandbox_relay_request_duration_seconds_count{error_type="too-many-requests",relay="inference"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("exposition lacks %s", line)
		}
	}
}

// An agent repeating a refused call logs once per route and window; the rest is counted
// into one summary when the window closes, and routes beyond the bound share one key.
func TestRelayRefusalLogIsSampled(t *testing.T) {
	c, log, _ := observed(t, false)
	refusals := newRefusalLog(c.logger())
	record := c.relayRecord("multica", refusals)
	for i := range 50 {
		req := httptest.NewRequest(http.MethodGet, "/api/issues/"+strconv.Itoa(i)+"/comments", nil)
		record(req, "", 0, relay.ErrPolicy, time.Millisecond)
	}
	record(httptest.NewRequest(http.MethodGet, "/api/issues/1/comments", nil), "", 403, nil, time.Millisecond)
	if events := lines(t, log); len(events) != 2 || events[0]["route"] != "/api/issues/{id}/comments" || events[0]["path"] != "/api/issues/0/comments" || events[1]["status"] != 403.0 {
		t.Fatalf("window events: %v", events)
	}
	log.Reset()
	refusals.flush()
	refusals.flush()
	events := lines(t, log)
	if len(events) != 1 || events[0]["msg"] != "relay events suppressed" || events[0]["count"] != 49.0 || events[0]["error_type"] != "policy-denied" || events[0]["route"] != "/api/issues/{id}/comments" {
		t.Fatalf("summary: %v", events)
	}
	log.Reset()
	for i := range relayLogKeys + 10 {
		record(httptest.NewRequest(http.MethodGet, "/api/r"+strconv.Itoa(i)+"x", nil), "", 404, nil, time.Millisecond)
	}
	if events := lines(t, log); len(events) != relayLogKeys+1 {
		t.Fatalf("bounded keys: %d events", len(events))
	}
	log.Reset()
	refusals.flush()
	if events := lines(t, log); len(events) != 1 || events[0]["route"] != "*" || events[0]["count"] != 9.0 {
		t.Fatalf("overflow summary: %v", events)
	}
}

// Paths keep their shape for diagnosis; tokens and JWTs are redacted and identifiers
// collapse in the route.
func TestRedactPath(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"
	for _, c := range []struct{ in, path, route string }{
		{"/api/issues/ABC-12/comments", "/api/issues/ABC-12/comments", "/api/issues/{id}/comments"},
		{"/api/issues/10000000-0000-4000-8000-000000000001", "/api/issues/10000000-0000-4000-8000-000000000001", "/api/issues/{id}"},
		{"/api/v4/projects/team/fixture/merge_requests/7", "/api/v4/projects/team/fixture/merge_requests/7", "/api/v4/projects/team/fixture/merge_requests/{id}"},
		{"/repos/o/r/commits/0123abc", "/repos/o/r/commits/0123abc", "/repos/o/r/commits/{id}"},
		{"/http/svc/hooks/" + jwt, "/http/svc/hooks/{redacted}", "/http/svc/hooks/{redacted}"},
		{"/http/svc/k/glpat-abcdef", "/http/svc/k/{redacted}", "/http/svc/k/{redacted}"},
		{"/x/msg_" + strings.Repeat("a", 64), "/x/{redacted}", "/x/{redacted}"},
		{"/x/Ab3dEf7hIj9kLm1nOp2qRs4tUv6wXy8z", "/x/{redacted}", "/x/{redacted}"},
		{"/team/a-very-long-repository-name-without-digits.git/info/refs", "/team/a-very-long-repository-name-without-digits.git/info/refs", "/team/a-very-long-repository-name-without-digits.git/info/refs"},
		{"/docs", "/docs", "/docs"},
	} {
		if path, route := redactPath(c.in); path != c.path || route != c.route {
			t.Errorf("%s: %s %s", c.in, path, route)
		}
	}
}

// errSecret stands for a native cause the log must not show.
var errSecret = &execution.AgentFailure{Message: "secret native detail"}

func observed(t *testing.T, debug bool) (Config, *bytes.Buffer, *metrics.Metrics) {
	t.Helper()
	m, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	return Config{Debug: debug, log: newLogger(&log, debug), metrics: m}, &log, m
}

func lines(t *testing.T, log *bytes.Buffer) []map[string]any {
	t.Helper()
	var events []map[string]any
	for line := range strings.Lines(log.String()) {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		events = append(events, event)
	}
	return events
}

func exposition(m *metrics.Metrics) string {
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metrics.Path, nil))
	return rec.Body.String()
}
