package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

// Relay refusals are recorded and, with debug, logged without query or headers.
func TestRelayRecordLogsRefusalsAtDebug(t *testing.T) {
	c, log, m := observed(t, true)
	record := c.relayRecord("inference")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=secret", nil)
	req.Header.Set("Authorization", "Bearer opaque-secret")
	record(req, 200, nil, time.Second)
	record(req, 0, relay.ErrPolicy, time.Millisecond)
	record(req, 429, nil, time.Second)
	events := lines(t, log)
	if len(events) != 2 || events[0]["level"] != "DEBUG" || events[0]["error_type"] != "policy-denied" || events[1]["status"] != 429.0 || events[1]["path"] != "/v1/chat/completions" {
		t.Fatalf("events: %v", events)
	}
	if strings.Contains(log.String(), "secret") {
		t.Fatal("query or credential logged")
	}
	text := exposition(m)
	for _, line := range []string{
		`sandbox_relay_request_duration_seconds_count{relay="inference",status_code="429"} 1`,
		`sandbox_relay_request_duration_seconds_count{error_type="policy-denied",relay="inference"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("exposition lacks %s", line)
		}
	}
	quiet, quietLog, _ := observed(t, false)
	quiet.relayRecord("git")(req, 0, relay.ErrPolicy, time.Millisecond)
	if quietLog.Len() != 0 {
		t.Fatalf("debug event without debug: %s", quietLog)
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
