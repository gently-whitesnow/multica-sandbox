package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func nativeEvent(t *testing.T, id, kind, session string, data map[string]any) httpEvent {
	t.Helper()
	if data == nil {
		data = map[string]any{}
	}
	data["sessionID"] = session
	raw, _ := json.Marshal(map[string]any{"id": id, "type": kind, "created": 1000, "data": data})
	var event httpEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestHTTPEventsOwnershipDedupAndToolVisibility(t *testing.T) {
	r := &running{events: &eventStream{}}
	h := newHTTPTurn(r, "ses_root")
	h.consume(nativeEvent(t, "1", "session.text.delta", "ses_foreign", map[string]any{"delta": "foreign"}))
	h.consume(nativeEvent(t, "2", "session.created", "ses_child", map[string]any{"parentID": "ses_root"}))
	text := nativeEvent(t, "3", "session.text.delta", "ses_child", map[string]any{"assistantMessageID": "m1", "delta": "child"})
	h.consume(text)
	h.consume(text)
	h.consume(nativeEvent(t, "4", "session.text.ended", "ses_child", map[string]any{"assistantMessageID": "m1", "text": "child completed"}))
	h.consume(nativeEvent(t, "root", "session.text.delta", "ses_root", map[string]any{"assistantMessageID": "mr", "delta": "root"}))
	base := map[string]any{"assistantMessageID": "m2", "id": "call", "name": "shell"}
	h.consume(nativeEvent(t, "5", "session.tool.input.started", "ses_root", base))
	h.consume(nativeEvent(t, "6", "session.tool.called", "ses_root", map[string]any{"assistantMessageID": "m2", "id": "call", "input": map[string]any{"command": "sleep 10"}}))
	if r.events.running.Load() != 1 || len(r.events.batch) != 2 || r.events.batch[1].Type != "tool_use" {
		t.Fatalf("tool was not visible while running: %+v", r.events.batch)
	}
	h.consume(nativeEvent(t, "7", "session.tool.success", "ses_root", map[string]any{"assistantMessageID": "m2", "id": "call", "content": []map[string]string{{"type": "text", "text": "done"}}}))
	if r.events.running.Load() != 0 || r.events.tools.Load() != 1 {
		t.Fatal("tool counters did not settle")
	}
	output, _, _ := r.events.result()
	if output != "root" || r.events.batch[2].Output != "done" {
		t.Fatalf("output=%q messages=%+v", output, r.events.batch)
	}
}

func TestHTTPEventsUsageAndBackgroundRequireLaterTerminal(t *testing.T) {
	r := &running{events: &eventStream{}}
	h := newHTTPTurn(r, "ses_root")
	h.consume(nativeEvent(t, "1", "session.execution.started", "ses_root", nil))
	h.consume(nativeEvent(t, "2", "session.step.ended", "ses_root", map[string]any{"tokens": map[string]any{"input": 11, "output": 7, "reasoning": 3, "cache": map[string]int{"read": 2, "write": 1}}}))
	h.consume(nativeEvent(t, "3", "session.shell.started", "ses_root", map[string]any{"shell": map[string]string{"id": "shell1"}}))
	h.consume(nativeEvent(t, "4", "session.execution.succeeded", "ses_root", nil))
	_, _, terminal, _, _, shells, _ := h.status()
	if !terminal || shells != 1 {
		t.Fatal("background shell must retain attempt")
	}
	ended := nativeEvent(t, "5", "session.shell.ended", "ses_root", map[string]any{"shell": map[string]string{"id": "shell1"}})
	ended.Created = 1001
	h.consume(ended)
	_, _, terminal, _, _, shells, _ = h.status()
	if terminal || shells != 0 {
		t.Fatal("shell completion must wait for resumed execution terminal")
	}
	h.consume(nativeEvent(t, "6", "session.execution.started", "ses_root", nil))
	final := nativeEvent(t, "7", "session.execution.succeeded", "ses_root", nil)
	final.Created = 1002
	h.consume(final)
	_, _, terminal, _, _, _, _ = h.status()
	if !terminal || r.spent.usage != (multica.Usage{Input: 11, Output: 10, CacheRead: 2, CacheWrite: 1}) {
		t.Fatalf("terminal=%v usage=%+v", terminal, r.spent.usage)
	}
}

func TestHTTPEventStreamMalformedAndDisconnected(t *testing.T) {
	for _, input := range []string{"", "data: {bad}\n\n", "data: " + strings.Repeat("x", (4<<20)+1)} {
		if err := readHTTPEvents(context.Background(), strings.NewReader(input), func(httpEvent) {}); err == nil {
			t.Fatal("unsafe stream accepted")
		}
	}
	var types []string
	err := readHTTPEvents(context.Background(), strings.NewReader("event: message\ndata: {\"type\":\"server.connected\",\ndata: \"data\":{}}\n\n"), func(e httpEvent) { types = append(types, e.Type) })
	if !errors.Is(err, streamCut) || len(types) != 1 || types[0] != "server.connected" {
		t.Fatalf("types=%v err=%v", types, err)
	}
}

func TestHTTPFailureNeverExposesProviderBody(t *testing.T) {
	err := nativeHTTPFailure(json.RawMessage(`{"type":"provider.rate-limit","status":429,"message":"secret-password","response":{"body":"private"}}`))
	var failure *execution.AgentFailure
	if !errors.As(err, &failure) || failure.Status != 429 || strings.Contains(err.Error(), "secret-password") || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe failure: %v", err)
	}
}
