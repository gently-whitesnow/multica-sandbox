package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
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

func TestNativeMessagesFollowsCursorWithoutOrder(t *testing.T) {
	const cursor = "next+/=&?"
	reads := 0
	r := &running{task: safeTask(), adapter: &Adapter{}, workload: &stubWorkload{api: func(path string) string {
		reads++
		u, err := url.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("limit") != strconv.Itoa(followPage) {
			t.Fatalf("page limit: %s", path)
		}
		var messages []map[string]any
		next := ""
		switch reads {
		case 1:
			if q.Get("order") != "desc" || q.Has("cursor") {
				t.Fatalf("first page: %s", path)
			}
			for i := followPage; i > 0; i-- {
				messages = append(messages, map[string]any{"id": strconv.Itoa(i), "time": map[string]any{"created": i}})
			}
			next = cursor
		case 2:
			// OpenCode rejects order with cursor, whose payload already carries order.
			if q.Has("order") || q.Get("cursor") != cursor {
				t.Fatalf("cursor page: %s", path)
			}
			messages = []map[string]any{{"id": "old", "time": map[string]any{"created": 0}}}
		default:
			t.Fatal("read beyond the last page")
		}
		data, _ := json.Marshal(map[string]any{"data": messages, "cursor": map[string]any{"next": next}})
		return string(data)
	}}}
	got, err := r.nativeMessages(context.Background(), "ses_fixture", 0)
	if err != nil || reads != 2 || len(got) != followPage+1 || got[0].ID != "old" || got[len(got)-1].ID != strconv.Itoa(followPage) {
		t.Fatalf("pagination: %d reads, %d messages, %v", reads, len(got), err)
	}
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

// An unreadable session is the sandbox's failure: Multica retries it, and the controller
// log keeps the read's cause with its redacted stderr.
func TestBackgroundShellUnreadableSessionIsRetriedAndLogged(t *testing.T) {
	var logs bytes.Buffer
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn"}
	workload := &stubWorkload{stderr: "ENOSPC: no space left on device, open '/workspace/data/opencode/log/a.log' token=abcdef0123456789\n", api: func(string) string { return "not json" }}
	r := &running{adapter: &Adapter{Log: slog.New(slog.NewTextHandler(&logs, nil))}, workload: workload}
	err := turnWith(t, context.Background(), agent, r)
	var host *execution.HostFailure
	if !errors.Is(err, errFollow) || !errors.As(err, &host) {
		t.Fatalf("unreadable session: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "opencode session unreadable") || !strings.Contains(out, "ENOSPC") || strings.Contains(out, "abcdef0123456789") {
		t.Fatalf("log: %s", out)
	}
}

// A read that failed for memory keeps the attempt's memory failure.
func TestBackgroundShellUnreadableSessionKeepsMemoryFailures(t *testing.T) {
	r := &running{adapter: &Adapter{}}
	for _, cause := range []error{&execution.OOMError{Killed: 1, Pool: true}, &execution.MemoryStallError{Stall: 90}} {
		if err := r.unreadable(cause); err != cause {
			t.Fatalf("%T became %v", cause, err)
		}
	}
	if err := r.unreadable(errors.New("exit status 1")); !errors.Is(err, errFollow) {
		t.Fatalf("read failure: %v", err)
	}
}

// A resumed call whose input is still a string reads as no input instead of failing
// every session read; the settled call reports its input.
func TestBackgroundShellReadsStringToolInput(t *testing.T) {
	later := time.Now().Add(time.Hour)
	note := map[string]any{"id": "msg_note", "type": "synthetic", "time": at(later), "metadata": map[string]any{"source": "shell", "shellID": "sh_1"}}
	step := func(status string, input any) map[string]any {
		return map[string]any{"id": "msg_step", "type": "assistant", "time": at(later.Add(time.Second)), "content": []any{
			map[string]any{"type": "tool", "id": "call-3", "name": "write", "state": map[string]any{"status": status, "input": input}},
		}}
	}
	api := &sessionAPI{pages: func(read int) []map[string]any {
		if read <= followFailures {
			return []map[string]any{step("pending", `{"filePath":"a.t`), note}
		}
		return []map[string]any{{"id": "msg_idle", "type": "idle", "time": at(later.Add(2 * time.Second))}, step("completed", map[string]any{"filePath": "a.txt"}), note}
	}}
	reporter := &reportStub{}
	r := &running{events: &eventStream{reporter: reporter}, workload: &stubWorkload{api: api.serve}}
	agent := &fakeAgent{version: "2.2.5", updates: backgroundUpdates(), stop: "end_turn"}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	for _, m := range reporter.messages {
		if m.Type == "tool_use" && m.CallID == "call-3" {
			input = m.Input
		}
	}
	if input["filePath"] != "a.txt" || api.reads <= followFailures {
		t.Fatalf("settled input %v after %d reads", input, api.reads)
	}
}
