package opencode

import (
	"context"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func TestACPStartupDoesNotCreateTranscriptRows(t *testing.T) {
	for _, surface := range []string{"issue", "chat"} {
		for _, mode := range []string{"fresh", "resumed", "lost"} {
			t.Run(surface+"/"+mode, func(t *testing.T) {
				task := relayTask(t)
				if surface == "chat" {
					task.ChatSessionID, task.IssueID = task.IssueID, ""
				}
				resume := ""
				if mode == "resumed" || mode == "lost" {
					resume = "ses_prior"
				}
				// A silent but metered turn must still pin its session, without a
				// startup row. Reusing the task exercises a second attempt too.
				for attempt := range 2 {
					reporter := &reportStub{}
					agent := &fakeAgent{version: "2.2.2", stop: "end_turn", loadFails: mode == "lost", usage: map[string]any{"inputTokens": 1, "totalTokens": 1}}
					if attempt == 1 {
						// These words remain valid agent output: no text filter.
						agent.updates = []map[string]any{{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Experimental OpenCode attempt started.running"}}}
					}
					r := &running{task: task, resume: resume, events: &eventStream{reporter: reporter}}
					if err := turnWith(t, context.Background(), agent, r); err != nil {
						t.Fatal(err)
					}
					if len(reporter.sessions) != 1 || len(reporter.messages) != attempt {
						t.Fatalf("sessions=%v messages=%+v", reporter.sessions, reporter.messages)
					}
					if attempt == 1 {
						m := reporter.messages[0]
						if m.Seq != 1 || m.Type != "text" || m.Content != "Experimental OpenCode attempt started.running" || m.IdempotencyKey == "" {
							t.Fatalf("agent output changed: %+v", m)
						}
					}
				}
			})
		}
	}
}

func TestOpenedSessionIsReportedBeforeAnyAgentOutput(t *testing.T) {
	reporter := &reportStub{}
	r := &running{events: &eventStream{reporter: reporter}}
	if _, err := r.opened(context.Background(), &acpClient{}, "ses_fresh"); err != nil {
		t.Fatal(err)
	}
	if err := r.events.flush(); err != nil || len(reporter.sessions) != 1 || len(reporter.messages) != 0 {
		t.Fatalf("session=%v messages=%+v error=%v", reporter.sessions, reporter.messages, err)
	}
	// Useful status events remain reportable if a future adapter needs them.
	r.events.add(multica.Message{Type: "status", Content: "useful progress"})
	if err := r.events.flush(); err != nil || len(reporter.messages) != 1 {
		t.Fatalf("progress lost: %+v %v", reporter.messages, err)
	}
}
