package opencode

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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
func TestReportingOutageDoesNotFailTask(t *testing.T) {
	agent := &fakeAgent{version: "2.2.2", updates: toolUpdates(), stop: "end_turn"}
	r := &running{events: &eventStream{reporter: &reportStub{fail: true}}}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal("reporting outage failed a completed run", err)
	}
	if output, _, _ := r.events.result(); output != "answer" {
		t.Fatalf("output %q", output)
	}
}

func TestUnattributedUsageUsesUnknownModel(t *testing.T) {
	reporter := &reportStub{}
	(&eventStream{reporter: reporter}).reportUsage(multica.Usage{Input: 1})
	if len(reporter.usage) != 1 || reporter.usage[0].Model != "unknown" {
		t.Fatalf("usage: %+v", reporter.usage)
	}
}

func TestChunksJoinUntilTheNextKind(t *testing.T) {
	stream := &eventStream{}
	stream.chunk("text", "a")
	stream.chunk("text", "b")
	stream.chunk("thinking", "c")
	stream.chunk("text", "d")
	if len(stream.batch) != 3 || stream.batch[0].Content != "ab" || stream.batch[2].Content != "d" || stream.output.String() != "abd" {
		t.Fatalf("batch: %+v", stream.batch)
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

func TestIdleWatchdogIsConfigurable(t *testing.T) {
	if (&Adapter{}).idleWatchdog() != 10*time.Minute || (&Adapter{IdleWatchdog: time.Hour}).idleWatchdog() != time.Hour {
		t.Fatal("idle watchdog ignores the configuration or the upstream default")
	}
}
