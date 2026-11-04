package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func init() { followInterval = 5 * time.Millisecond }

// backgroundUpdates launch one shell that OpenCode moves to the background.
func backgroundUpdates() []map[string]any {
	return []map[string]any{
		{"sessionUpdate": "tool_call", "toolCallId": "call-bg", "title": "shell", "kind": "execute", "status": "pending", "rawInput": map[string]any{}},
		{"sessionUpdate": "tool_call_update", "toolCallId": "call-bg", "status": "in_progress", "rawInput": map[string]any{"command": "make check", "background": true}},
		{"sessionUpdate": "tool_call_update", "toolCallId": "call-bg", "status": "completed", "rawOutput": map[string]any{"metadata": map[string]any{"status": "running", "truncated": false, "shellID": "sh_1"}},
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "Command moved to the background (shell ID: sh_1)."}}}},
		{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "started; "}},
	}
}

// sessionAPI answers session message pages, newest first, from what pages returns per read.
type sessionAPI struct {
	mu    sync.Mutex
	reads int
	paths []string
	pages func(read int) []map[string]any
}

func (a *sessionAPI) serve(path string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	a.paths = append(a.paths, path)
	data, _ := json.Marshal(map[string]any{"data": a.pages(a.reads), "cursor": map[string]any{}})
	return string(data)
}

func at(t time.Time) map[string]any {
	return map[string]any{"created": t.UnixMilli(), "completed": t.UnixMilli()}
}

func TestBackgroundShellEndsTheAttemptWithTheResumedTurn(t *testing.T) {
	later := time.Now().Add(time.Hour)
	resumed := []map[string]any{
		{"id": "msg_idle", "type": "idle", "time": at(later.Add(3 * time.Second))},
		{"id": "msg_resumed", "type": "assistant", "time": at(later.Add(time.Second)), "tokens": map[string]any{"input": 5, "output": 2, "reasoning": 1, "cache": map[string]any{"read": 3}},
			"content": []any{
				map[string]any{"type": "tool", "id": "call-2", "name": "shell", "state": map[string]any{"status": "completed", "input": map[string]any{"command": "cat log"}, "content": []any{map[string]any{"type": "text", "text": "ok"}}}},
				map[string]any{"type": "text", "text": "checks passed"},
			}},
		{"id": "msg_note", "type": "synthetic", "time": at(later), "metadata": map[string]any{"source": "shell", "shellID": "sh_1", "state": "completed"}},
	}
	api := &sessionAPI{pages: func(read int) []map[string]any {
		if read < 3 {
			return nil
		}
		return resumed
	}}
	reporter := &reportStub{}
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn", usage: map[string]any{"inputTokens": 10, "outputTokens": 4}}
	r := &running{events: &eventStream{reporter: reporter}, workload: &stubWorkload{api: api.serve}}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	output, _, usage := r.events.result()
	if output != "started; checks passed" || usage != (multica.Usage{Input: 15, Output: 7, CacheRead: 3}) {
		t.Fatalf("resumed turn: %q %+v", output, usage)
	}
	if api.reads < 3 || r.events.background.Load() != 0 || !strings.Contains(api.paths[0], "/api/session/ses_fresh/message?") {
		t.Fatalf("follow: %d reads %v, %d shells", api.reads, api.paths, r.events.background.Load())
	}
	var tools []string
	for _, m := range reporter.messages {
		if m.Type == "tool_result" {
			tools = append(tools, m.CallID+"="+m.Output)
		}
	}
	if strings.Join(tools, ",") != "call-bg=Command moved to the background (shell ID: sh_1).,call-2=ok" {
		t.Fatalf("tool results: %v", tools)
	}
}

func TestBackgroundShellWaitsWhileResumedTurnRuns(t *testing.T) {
	later := time.Now().Add(time.Hour)
	streaming := map[string]any{"created": later.Add(time.Second).UnixMilli()}
	api := &sessionAPI{pages: func(read int) []map[string]any {
		note := map[string]any{"id": "msg_note", "type": "synthetic", "time": at(later), "metadata": map[string]any{"source": "shell", "shellID": "sh_1"}}
		if read < 4 {
			// The resumed step is still streaming: no idle yet.
			return []map[string]any{{"id": "msg_step", "type": "assistant", "time": streaming}, note}
		}
		return []map[string]any{
			{"id": "msg_idle", "type": "idle", "time": at(later.Add(2 * time.Second))},
			{"id": "msg_step", "type": "assistant", "time": at(later.Add(time.Second)), "content": []any{map[string]any{"type": "text", "text": "done"}}},
			note,
		}
	}}
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn"}
	r := &running{workload: &stubWorkload{api: api.serve}}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	if output, _, _ := r.events.result(); output != "started; done" || api.reads != 4 {
		t.Fatalf("output %q after %d reads", output, api.reads)
	}
}

func TestBackgroundShellFinishedWithinTheTurnEndsAtOnce(t *testing.T) {
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn", pause: 20 * time.Millisecond}
	api := &sessionAPI{}
	api.pages = func(int) []map[string]any {
		prompted, _ := agent.calledAt("session/prompt")
		return []map[string]any{{"id": "msg_note", "type": "synthetic", "time": at(prompted.Add(5 * time.Millisecond)), "metadata": map[string]any{"source": "shell", "shellID": "sh_1"}}}
	}
	r := &running{workload: &stubWorkload{api: api.serve}}
	if err := turnWith(t, context.Background(), agent, r); err != nil || api.reads != 1 {
		t.Fatalf("follow: %v after %d reads", err, api.reads)
	}
}

func TestBackgroundShellUnreadableSessionFails(t *testing.T) {
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn"}
	r := &running{workload: &stubWorkload{api: func(string) string { return "not json" }}}
	if err := turnWith(t, context.Background(), agent, r); !errors.Is(err, errFollow) {
		t.Fatalf("unreadable session: %v", err)
	}
}

func TestBackgroundShellCountsAsRunningTool(t *testing.T) {
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn"}
	ctx, cancel := context.WithCancel(context.Background())
	api := &sessionAPI{pages: func(int) []map[string]any { return nil }}
	r := &running{events: &eventStream{}, workload: &stubWorkload{api: api.serve}}
	done := make(chan error, 1)
	go func() { done <- turnWith(t, ctx, agent, r) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.events.background.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.events.active.Store(time.Now().Add(-time.Hour).UnixNano())
	if after, idle := r.events.idle(time.Minute, 2*time.Hour); idle || after != 2*time.Hour {
		t.Fatal("a background shell left the idle window in force")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled follow: %v", err)
	}
}

func TestBackgroundShellNeedsOpenCodeTwo(t *testing.T) {
	agent := &fakeAgent{version: "1.18.35", updates: backgroundUpdates(), stop: "end_turn"}
	r := &running{workload: &stubWorkload{api: func(string) string { t.Fatal("1.x session read"); return "" }}}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundShellResumedToolCountsAsRunning(t *testing.T) {
	later := time.Now().Add(time.Hour)
	note := map[string]any{"id": "msg_note", "type": "synthetic", "time": at(later), "metadata": map[string]any{"source": "shell", "shellID": "sh_1"}}
	step := func(status string) map[string]any {
		return map[string]any{"id": "msg_step", "type": "assistant", "time": at(later.Add(time.Second)), "content": []any{
			map[string]any{"type": "tool", "id": "call-3", "name": "shell", "state": map[string]any{"status": status, "input": map[string]any{"command": "make check"}, "content": []any{map[string]any{"type": "text", "text": "passed"}}}},
		}}
	}
	r := &running{events: &eventStream{}}
	var running []int64
	api := &sessionAPI{pages: func(read int) []map[string]any {
		running = append(running, r.events.running.Load())
		if read < 3 {
			return []map[string]any{step("running"), note}
		}
		return []map[string]any{{"id": "msg_idle", "type": "idle", "time": at(later.Add(2 * time.Second))}, step("completed"), note}
	}}
	r.workload = &stubWorkload{api: api.serve}
	reporter := &reportStub{}
	r.events.reporter = reporter
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn"}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	if running[0] != 0 || running[2] != 1 || r.events.running.Load() != 0 {
		t.Fatalf("running tools per read: %v, after: %d", running, r.events.running.Load())
	}
	last := reporter.messages[len(reporter.messages)-1]
	if last.Type != "tool_result" || last.CallID != "call-3" || last.Output != "passed" {
		t.Fatalf("resumed tool result: %+v", last)
	}
}

func TestBackgroundShellFailureReportsTheAnsweredTurnOnce(t *testing.T) {
	reporter := &reportStub{}
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn", usage: map[string]any{"inputTokens": 10, "outputTokens": 4}}
	r := &running{events: &eventStream{reporter: reporter}, workload: &stubWorkload{api: func(string) string { return "not json" }}}
	if err := turnWith(t, context.Background(), agent, r); !errors.Is(err, errFollow) {
		t.Fatalf("unreadable session: %v", err)
	}
	if len(reporter.usage) != 1 || reporter.usage[0].Input != 10 || reporter.usage[0].Output != 4 {
		t.Fatalf("usage %+v", reporter.usage)
	}
}
