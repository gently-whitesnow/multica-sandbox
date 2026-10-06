package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type reportStub struct {
	messages []multica.Message
	usage    []multica.Usage
	sessions []string
	fail     bool
}

func (r *reportStub) ReportMessages(_ context.Context, _ string, m []multica.Message) error {
	r.messages = append(r.messages, m...)
	if r.fail {
		return fmt.Errorf("report unavailable")
	}
	return nil
}
func (r *reportStub) ReportUsage(_ context.Context, _ string, u multica.Usage) error {
	r.usage = append(r.usage, u)
	return nil
}
func (r *reportStub) ReportSession(_ context.Context, _, s string) error {
	r.sessions = append(r.sessions, s)
	return nil
}
func native(typ, id string, part map[string]any) string {
	part["id"] = id
	data, _ := json.Marshal(map[string]any{"type": typ, "sessionID": "ses_fixture", "timestamp": 1234, "part": part})
	return string(data) + "\n"
}
func successfulStream() string {
	return native("step_start", "p1", map[string]any{}) + native("tool_use", "p2", map[string]any{"tool": "docs_read", "callID": "opaque-call", "state": map[string]any{"status": "completed", "input": map[string]string{"id": "doc"}, "output": "tool result"}}) + native("step_finish", "p3", map[string]any{"reason": "tool-calls", "tokens": map[string]any{"input": 10, "output": 2, "reasoning": 3, "cache": map[string]int{"read": 4, "write": 5}}}) + native("step_start", "p4", map[string]any{}) + native("text", "p5", map[string]any{"text": "answer"}) + native("step_finish", "p6", map[string]any{"reason": "stop", "tokens": map[string]int{"input": 20, "output": 7}})
}
func TestNativeReportsOrderedMessagesCumulativeUsageSession(t *testing.T) {
	reporter := &reportStub{}
	stream := eventStream{reporter: reporter, seq: 1, usage: multica.Usage{Provider: "managed-inference", Model: "selected"}}
	if err := stream.read(context.Background(), strings.NewReader(successfulStream())); err != nil {
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
	if reporter.messages[1].CallID != "opaque-call" || reporter.messages[2].CallID != "opaque-call" || reporter.messages[2].Type != "tool_result" {
		t.Fatal("tool correlation lost")
	}
	if len(reporter.usage) != 2 || reporter.usage[1].Input != 30 || reporter.usage[1].Output != 12 || reporter.usage[1].CacheRead != 4 || reporter.usage[1].CacheWrite != 5 {
		t.Fatalf("incorrect cumulative usage: %+v", reporter.usage)
	}
}
func TestNativeStreamRejectsFalseCompletion(t *testing.T) {
	start := native("step_start", "s", map[string]any{})
	for _, data := range []string{"", "garbage\n", start, start + native("step_finish", "f", map[string]any{"reason": "tool-calls"}), start + native("step_finish", "f", map[string]any{"reason": "stop"}), start + native("step_finish", "f", map[string]any{"reason": "stop", "tokens": map[string]int{"input": -1}}), strings.Replace(successfulStream(), "ses_fixture", "wrong", 1), strings.Repeat("x", 1<<20) + "\n"} {
		if err := (&eventStream{}).read(context.Background(), strings.NewReader(data)); err == nil {
			t.Fatal("accepted incomplete/malformed native stream")
		}
	}
}
func TestNativeZeroExitErrorRedactsBodies(t *testing.T) {
	for _, status := range []int{403, 429} {
		data := fmt.Sprintf(`{"type":"error","sessionID":"ses_fixture","error":{"name":"APIError","data":{"statusCode":%d,"message":"secret","responseHeaders":{"authorization":"secret"}}}}`, status) + "\n"
		err := (&eventStream{}).read(context.Background(), strings.NewReader(data))
		var failure *execution.AgentFailure
		if !errors.As(err, &failure) || failure.Status != status || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe native error: %v", err)
		}
	}
}
func TestNativeReportingOutageStopsStream(t *testing.T) {
	reporter := &reportStub{fail: true}
	if err := (&eventStream{reporter: reporter}).read(context.Background(), strings.NewReader(successfulStream())); err == nil || len(reporter.messages) != 1 {
		t.Fatal("reporting outage ignored")
	}
}
func TestNativeDuplicatePartsDoNotDoubleUsage(t *testing.T) {
	data := successfulStream()
	last := native("step_finish", "p6", map[string]any{"reason": "stop", "tokens": map[string]int{"input": 20, "output": 7}})
	stream := eventStream{}
	if err := stream.read(context.Background(), strings.NewReader(data+last)); err != nil || stream.usage.Input != 30 {
		t.Fatalf("duplicate usage: %v %+v", err, stream.usage)
	}
}
func TestRedactionRetainsRotatedTokens(t *testing.T) {
	r := &running{}
	r.remember("old-token")
	r.remember("new-token")
	reporter := &reportStub{}
	stream := eventStream{reporter: reporter, redact: r.redact}
	data := strings.Replace(successfulStream(), "answer", "old-token new-token", 1)
	if err := stream.read(context.Background(), strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(reporter.messages)
	if strings.Contains(string(encoded), "old-token") || strings.Contains(stream.output.String(), "new-token") {
		t.Fatal("projection token leaked")
	}
}

func TestNativeRejectsUnboundedOrMissingPartIdentity(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("p", 257)} {
		data := native("step_start", id, map[string]any{}) + successfulStream()
		if err := (&eventStream{}).read(context.Background(), strings.NewReader(data)); err == nil {
			t.Fatal("unbounded/missing part identity accepted")
		}
	}
}
