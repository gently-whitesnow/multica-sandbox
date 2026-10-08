package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type reportStub struct {
	mu       sync.Mutex
	messages []multica.Message
	usage    []multica.Usage
	sessions []string
	fail     bool
}

func (r *reportStub) ReportMessages(_ context.Context, _ string, m []multica.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return fmt.Errorf("report unavailable")
	}
	r.messages = append(r.messages, m...)
	return nil
}
func (r *reportStub) ReportUsage(_ context.Context, _ string, u multica.Usage) error {
	r.usage = append(r.usage, u)
	return nil
}
func (r *reportStub) ReportSession(_ context.Context, _, s, _ string) error {
	r.sessions = append(r.sessions, s)
	return nil
}
func native(typ string, part map[string]any) string {
	data, _ := json.Marshal(map[string]any{"type": typ, "sessionID": "ses_fixture", "timestamp": 1234, "part": part})
	return string(data) + "\n"
}
func successfulStream() string {
	return native("step_start", map[string]any{}) + native("tool_use", map[string]any{"tool": "docs_read", "callID": "opaque-call", "state": map[string]any{"status": "completed", "input": map[string]string{"id": "doc"}, "output": "tool result"}}) + native("step_finish", map[string]any{"reason": "tool-calls", "tokens": map[string]any{"input": 10, "output": 2, "reasoning": 3, "cache": map[string]int{"read": 4, "write": 5}}}) + native("step_start", map[string]any{}) + native("text", map[string]any{"text": "answer"}) + native("step_finish", map[string]any{"reason": "stop", "tokens": map[string]int{"input": 20, "output": 7}})
}

func TestNativeReportsOrderedMessagesUsageSession(t *testing.T) {
	reporter := &reportStub{}
	stream := eventStream{reporter: reporter, model: "managed-inference/selected"}
	if err := stream.read(context.Background(), strings.NewReader("not json\n"+native("unknown_part", map[string]any{})+successfulStream())); err != nil {
		t.Fatal(err)
	}
	if stream.output.String() != "answer" || len(reporter.sessions) != 1 || reporter.sessions[0] != "ses_fixture" {
		t.Fatal("native result/session lost")
	}
	for i, m := range reporter.messages {
		if m.Seq != i+2 || m.CreatedAt.UnixMilli() != 1234 {
			t.Fatal("ordering/timestamp lost")
		}
	}
	if reporter.messages[1].CallID != "opaque-call" || reporter.messages[2].CallID != "opaque-call" || reporter.messages[2].Type != "tool_result" || *reporter.messages[2].OutputTruncated {
		t.Fatal("tool correlation lost")
	}
	want := multica.Usage{Provider: "opencode", Model: "managed-inference/selected", Input: 30, Output: 12, CacheRead: 4, CacheWrite: 5}
	if len(reporter.usage) != 1 || reporter.usage[0] != want {
		t.Fatalf("usage: %+v", reporter.usage)
	}
}

func TestUnattributedUsageUsesUnknownModel(t *testing.T) {
	reporter := &reportStub{}
	if err := (&eventStream{reporter: reporter}).read(context.Background(), strings.NewReader(successfulStream())); err != nil || reporter.usage[0].Model != "unknown" {
		t.Fatalf("usage: %v %+v", err, reporter.usage)
	}
}

func TestNativeStreamTerminalSignals(t *testing.T) {
	start := native("step_start", map[string]any{})
	text := native("text", map[string]any{"text": "done"})
	for name, tc := range map[string]struct {
		data, failure string
	}{
		"empty":             {"", "no step finished"},
		"open step":         {start + text, "step still open"},
		"tool-calls":        {start + text + native("step_finish", map[string]any{"reason": "tool-calls"}), "continuation that never started"},
		"stop after tool":   {start + native("tool_use", map[string]any{"callID": "c", "state": map[string]any{"status": "completed"}}) + native("step_finish", map[string]any{"reason": "stop"}), "continuation that never started"},
		"void step":         {start + native("step_finish", map[string]any{"reason": "stop", "tokens": map[string]int{"input": 0}}), "empty step"},
		"reasoning only":    {start + native("reasoning", map[string]any{"text": "hm"}) + native("step_finish", map[string]any{"reason": "stop"}), "empty step"},
		"missing reason":    {start + text + native("step_finish", map[string]any{}), ""},
		"length reason":     {start + text + native("step_finish", map[string]any{"reason": "length"}), ""},
		"provider tool":     {start + native("tool_use", map[string]any{"callID": "c", "metadata": map[string]bool{"providerExecuted": true}, "state": map[string]any{"status": "completed"}}) + native("step_finish", map[string]any{"reason": "stop"}), ""},
		"usage-only step":   {start + native("step_finish", map[string]any{"reason": "stop", "cost": 0.01}), ""},
		"oversized ignored": {strings.Repeat("x", 2<<20) + "\n" + start + text + native("step_finish", map[string]any{"reason": "stop"}), ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := (&eventStream{}).read(context.Background(), strings.NewReader(tc.data))
			if tc.failure == "" && err != nil || tc.failure != "" && (err == nil || !strings.Contains(err.Error(), tc.failure)) {
				t.Fatalf("error=%v", err)
			}
			var agent *execution.AgentFailure
			if err != nil && (!errors.As(err, &agent) || !strings.HasPrefix(err.Error(), "opencode stream ended")) {
				t.Fatalf("not classifiable as provider_network: %v", err)
			}
		})
	}
}

func TestNativeZeroExitErrorWithholdsBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{{"APIError", 429, "HTTP 429"}, {"APIError", 403, "HTTP 403"}, {"ProviderAuthError", 0, "opencode reported ProviderAuthError"}, {"bad name with spaces", 0, "output withheld"}} {
		data := fmt.Sprintf(`{"type":"error","sessionID":"ses_fixture","error":{"name":%q,"data":{"statusCode":%d,"message":"secret","responseHeaders":{"authorization":"secret"}}}}`, tc.name, tc.status) + "\n"
		err := (&eventStream{}).read(context.Background(), strings.NewReader(data+successfulStream()))
		var failure *execution.AgentFailure
		if !errors.As(err, &failure) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe native error: %v", err)
		}
	}
}

func TestReportingOutageDoesNotFailTask(t *testing.T) {
	reporter := &reportStub{fail: true}
	stream := eventStream{reporter: reporter}
	if err := stream.read(context.Background(), strings.NewReader(successfulStream())); err != nil || stream.output.String() != "answer" {
		t.Fatal("reporting outage failed a completed run", err)
	}
}

func TestToolOutputPreviewIsUTF8Safe(t *testing.T) {
	reporter := &reportStub{}
	long := strings.Repeat("я", toolOutputPreview)
	data := native("step_start", map[string]any{}) + native("tool_use", map[string]any{"callID": "c", "state": map[string]any{"status": "completed", "output": long}}) + native("step_finish", map[string]any{"reason": "tool-calls"}) + native("step_start", map[string]any{}) + native("text", map[string]any{"text": "ok"}) + native("step_finish", map[string]any{"reason": "stop"})
	if err := (&eventStream{reporter: reporter}).read(context.Background(), strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	result := reporter.messages[2]
	if result.Type != "tool_result" || !*result.OutputTruncated || len(result.Output) > toolOutputPreview || !strings.HasPrefix(long, result.Output) {
		t.Fatalf("preview: %d %v", len(result.Output), *result.OutputTruncated)
	}
}

func TestIdleWatchdogTracksStreamActivity(t *testing.T) {
	var stream eventStream
	stream.touch()
	if stream.idle(time.Minute) {
		t.Fatal("fresh stream reported idle")
	}
	stream.active.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	if !stream.idle(time.Minute) {
		t.Fatal("silent stream not reported idle")
	}
}
