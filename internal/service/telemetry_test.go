package service

import (
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func TestFinishedLineSummarizesAttempts(t *testing.T) {
	claimed := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	task := multica.Task{ID: "task-1", WorkspaceID: "ws-1", AgentID: "agent-1", IssueID: "issue-1"}
	completed := controller.Finish{Task: task, Claimed: claimed, Started: claimed.Add(4800 * time.Millisecond), Ended: claimed.Add(15 * time.Second),
		Result: execution.Result{SessionID: "ses_1", Telemetry: execution.Telemetry{Model: "inference-codex/gpt-demo", Effort: "high", InputTokens: 1200, OutputTokens: 80, CachedTokens: 900, PeakMemory: 512 << 20}}}
	want := "finished task=task-1 workspace=ws-1 agent=agent-1 kind=issue outcome=completed prepare=4.8s run=10.2s total=15.0s " +
		"model=inference-codex/gpt-demo effort=high tokens_in=1200 tokens_out=80 tokens_cached=900 peak_memory=512MiB session=ses_1"
	if got := finished(completed); got != want {
		t.Fatalf("completed:\n got %s\nwant %s", got, want)
	}
	task.IssueID, task.ChatSessionID = "", "chat-1"
	rejected := controller.Finish{Task: task, Claimed: claimed, Ended: claimed.Add(time.Second), Deferred: true,
		Failure: &execution.RejectedError{Err: errSecret}}
	want = "finished task=task-1 workspace=ws-1 agent=agent-1 kind=chat outcome=failed reason=environment_prepare_failed total=1.0s report=deferred " +
		`error="Sandbox rejected the task before OpenCode started"`
	if got := finished(rejected); got != want {
		t.Fatalf("rejected:\n got %s\nwant %s", got, want)
	}
	agent := controller.Finish{Task: task, Claimed: claimed, Started: claimed, Ended: claimed, Failure: &execution.AgentFailure{Status: 429}}
	if got := finished(agent); got != "finished task=task-1 workspace=ws-1 agent=agent-1 kind=chat outcome=failed reason=agent prepare=0.0s run=0.0s total=0.0s "+
		`error="Agent inference request failed (HTTP 429)"` {
		t.Fatalf("agent failure: %s", got)
	}
}

// errSecret stands for a native cause the line must not show.
var errSecret = &execution.AgentFailure{Message: "secret native detail"}
