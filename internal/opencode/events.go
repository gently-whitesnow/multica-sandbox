package opencode

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Upstream daemon values for OpenCode transcripts.
const (
	toolOutputPreview = 8192
	reportInterval    = 500 * time.Millisecond
	reportTimeout     = 5 * time.Second
	idleWatchdog      = 10 * time.Minute
)

type Reporter interface {
	ReportMessages(context.Context, string, []multica.Message) error
	ReportUsage(context.Context, string, multica.Usage) error
	ReportSession(context.Context, string, string, string) error
}

// eventStream reports ACP session updates like the upstream daemon reports its agents.
type eventStream struct {
	reporter Reporter
	task     string
	model    string
	// workDir names the retained workdir reported with the session.
	workDir string
	active  atomic.Int64
	tools   atomic.Int64
	// running counts tools in progress; the idle watchdog pauses while any runs.
	running atomic.Int64

	mu      sync.Mutex
	output  strings.Builder
	session string
	usage   multica.Usage
	seq     int
	batch   []multica.Message
	pending string
}

// chunk appends streamed text or reasoning, joining consecutive chunks of one kind
// into a single transcript row until the next report.
func (s *eventStream) chunk(kind, text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	if kind == "text" {
		s.output.WriteString(text)
	}
	if n := len(s.batch); n > 0 && s.batch[n-1].Type == kind {
		s.batch[n-1].Content += text
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.add(multica.Message{Type: kind, Content: text, CreatedAt: time.Now().UTC()})
}

// begin records the native session, reported with the next batch.
func (s *eventStream) begin(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session, s.pending = session, session
}

func (s *eventStream) result() (output, session string, usage multica.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.String(), s.session, s.usage
}

// preview applies the upstream 8 KiB UTF-8 tool output preview.
func preview(output string) (string, bool) {
	if len(output) <= toolOutputPreview {
		return output, false
	}
	end := toolOutputPreview
	for !utf8.RuneStart(output[end]) {
		end--
	}
	return output[:end], true
}

func (s *eventStream) touch() { s.active.Store(time.Now().UnixNano()) }

// idle reports silence longer than the upstream OpenCode idle watchdog while no tool runs.
func (s *eventStream) idle(window time.Duration) bool {
	return s.running.Load() == 0 && time.Since(time.Unix(0, s.active.Load())) > window
}

// add sequences transcript rows; seq 1 is the controller's start message.
func (s *eventStream) add(messages ...multica.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range messages {
		s.seq++
		m.Seq = s.seq + 1
		s.batch = append(s.batch, m)
	}
}

// flushEvery reports batches in the background; stop sends the tail before the terminal callback.
func (s *eventStream) flushEvery(interval time.Duration) func() {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.flush()
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-exited
		s.flush()
	}
}

// flush is best-effort, like the upstream daemon: reporting outages never fail the task.
func (s *eventStream) flush() {
	s.mu.Lock()
	session, batch := s.pending, s.batch
	s.pending, s.batch = "", nil
	s.mu.Unlock()
	if s.reporter == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if session != "" {
		_ = s.reporter.ReportSession(ctx, s.task, session, s.workDir)
	}
	if len(batch) > 0 {
		_ = s.reporter.ReportMessages(ctx, s.task, batch)
	}
}

// reportUsage sends the turn total once; OpenCode emits no model, so attribution is the applied model.
func (s *eventStream) reportUsage(u multica.Usage) {
	s.mu.Lock()
	s.usage = u
	s.mu.Unlock()
	if s.reporter == nil || u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
		return
	}
	u.Provider, u.Model = "opencode", s.model
	if u.Model == "" {
		u.Model = "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	_ = s.reporter.ReportUsage(ctx, s.task, u)
}
