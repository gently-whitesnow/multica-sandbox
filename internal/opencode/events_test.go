package opencode

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
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
	// err refuses every report while set.
	err error
}

func (r *reportStub) ReportMessages(_ context.Context, _ string, m []multica.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return fmt.Errorf("report unavailable")
	}
	if r.err != nil {
		return r.err
	}
	r.messages = append(r.messages, m...)
	return nil
}
func (r *reportStub) ReportUsage(_ context.Context, _ string, u multica.Usage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.usage = append(r.usage, u)
	return nil
}
func (r *reportStub) ReportSession(_ context.Context, _, s, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sessions = append(r.sessions, s)
	return nil
}
func (r *reportStub) refuse(err error) { r.mu.Lock(); defer r.mu.Unlock(); r.err = err }

// loggedStream reports to reporter and logs into the returned buffer.
func loggedStream(reporter Reporter) (*eventStream, *bytes.Buffer) {
	var logs bytes.Buffer
	return &eventStream{reporter: reporter, log: slog.New(slog.NewJSONHandler(&logs, nil)), task: "task"}, &logs
}

func TestTransientReportFailuresRetryInOrder(t *testing.T) {
	reporter := &reportStub{err: &multica.HTTPError{Status: 503}}
	s, logs := loggedStream(reporter)
	s.begin("ses_1")
	s.add(multica.Message{Type: "text", Content: "a"})
	s.reportUsage(multica.Usage{Input: 1})
	if s.flush() == nil {
		t.Fatal("transient failure not kept")
	}
	s.add(multica.Message{Type: "text", Content: "b"})
	reporter.refuse(nil)
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if len(reporter.messages) != 2 || reporter.messages[0].Content != "a" || reporter.messages[0].Seq != 2 || reporter.messages[1].Seq != 3 ||
		len(reporter.usage) != 1 || len(reporter.sessions) != 1 || logs.Len() != 0 {
		t.Fatalf("messages=%+v usage=%v sessions=%v logs=%s", reporter.messages, reporter.usage, reporter.sessions, logs)
	}
}

// lostAck stores every batch like Multica but loses the first answer.
type lostAck struct {
	reportStub
	calls [][]multica.Message
}

func (r *lostAck) ReportMessages(_ context.Context, _ string, m []multica.Message) error {
	r.calls = append(r.calls, append([]multica.Message(nil), m...))
	if len(r.calls) == 1 {
		return context.DeadlineExceeded
	}
	return nil
}

func TestResentReportIsIdempotentAndUnchanged(t *testing.T) {
	reporter := &lostAck{}
	s, _ := loggedStream(reporter)
	s.chunk("text", "a")
	if s.flush() == nil {
		t.Fatal("lost answer not kept")
	}
	// Multica may hold row 2 already: new text must not change it.
	s.chunk("text", "b")
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	first, second := reporter.calls[0], reporter.calls[1]
	if len(first) != 1 || len(second) != 2 || !reflect.DeepEqual(second[0], first[0]) || second[1].Content != "b" || second[1].Seq != 3 {
		t.Fatalf("resent rows changed: %+v then %+v", first, second)
	}
	// Multica derives the row ID from the key: the replay stores nothing new.
	stored := map[string]string{}
	for _, call := range reporter.calls {
		for _, m := range call {
			if _, ok := stored[m.IdempotencyKey]; !ok && m.IdempotencyKey != "" {
				stored[m.IdempotencyKey] = m.Content
			}
		}
	}
	if len(stored) != 2 || stored[first[0].IdempotencyKey]+stored[second[1].IdempotencyKey] != "ab" {
		t.Fatalf("stored transcript %v", stored)
	}
	other, _ := loggedStream(&reportStub{})
	other.add(multica.Message{Type: "text"})
	if other.batch[0].IdempotencyKey == first[0].IdempotencyKey {
		t.Fatal("keys collide across attempts")
	}
}

func TestReportBacklogIsBoundedAndLogged(t *testing.T) {
	s, logs := loggedStream(&reportStub{err: &multica.HTTPError{Status: 503}})
	for range pendingLimit + 10 {
		s.add(multica.Message{Type: "tool_use"})
	}
	_ = s.flush()
	if len(s.batch) != pendingLimit || s.batch[0].Seq != 12 || !strings.Contains(logs.String(), `"messages":10`) {
		t.Fatalf("backlog=%d first=%d logs=%s", len(s.batch), s.batch[0].Seq, logs)
	}
	logs.Reset()
	s.flushEvery(time.Hour)()
	if len(s.batch) != 0 || !strings.Contains(logs.String(), fmt.Sprintf(`"messages":%d`, pendingLimit)) {
		t.Fatalf("final undelivered rows not logged: %s", logs)
	}
}

func TestRefusedReportIsDroppedAndLogged(t *testing.T) {
	s, logs := loggedStream(&reportStub{err: &multica.HTTPError{Status: 400}})
	s.add(multica.Message{Type: "text", Content: "a"})
	s.reportUsage(multica.Usage{Input: 1})
	if err := s.flush(); err != nil || len(s.batch) != 0 || s.unreported != nil || strings.Count(logs.String(), "attempt report dropped") != 2 {
		t.Fatalf("refused report kept or silent: %v %s", err, logs)
	}
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
