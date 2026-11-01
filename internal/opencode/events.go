package opencode

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
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
	// pendingLimit bounds transcript rows kept across failed reports; older rows go first.
	pendingLimit = 512
)

type Reporter interface {
	ReportMessages(context.Context, string, []multica.Message) error
	ReportUsage(context.Context, string, multica.Usage) error
	ReportSession(context.Context, string, string, string) error
}

// eventStream reports ACP session updates like the upstream daemon reports its agents.
type eventStream struct {
	reporter Reporter
	// log reports what Multica never received; nil discards.
	log   *slog.Logger
	task  string
	model string
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
	// unreported is turn usage a transient failure kept for the next report.
	unreported *multica.Usage
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
		if err := s.flush(); err != nil {
			s.mu.Lock()
			rows, usage := len(s.batch), s.unreported != nil
			s.batch, s.pending, s.unreported = nil, "", nil
			s.mu.Unlock()
			s.dropped(rows, usage, err)
		}
	}
}

// retryable failures keep their reports for the next flush.
func retryable(err error) bool {
	return multica.Transient(err) || errors.Is(err, context.DeadlineExceeded)
}

// flush is best-effort, like the upstream daemon: reporting outages never fail the task.
// Transiently refused reports are kept, within pendingLimit, for the next flush, which
// returns the last such failure; anything else Multica refused is dropped and logged.
func (s *eventStream) flush() (kept error) {
	s.mu.Lock()
	session, batch, usage := s.pending, s.batch, s.unreported
	s.pending, s.batch, s.unreported = "", nil, nil
	s.mu.Unlock()
	if s.reporter == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if session != "" {
		if err := s.reporter.ReportSession(ctx, s.task, session, s.workDir); retryable(err) {
			kept = err
			s.mu.Lock()
			if s.pending == "" {
				s.pending = session
			}
			s.mu.Unlock()
		}
	}
	if usage != nil {
		kept = cmp.Or(s.sendUsage(ctx, *usage), kept)
	}
	if len(batch) == 0 {
		return kept
	}
	err := s.reporter.ReportMessages(ctx, s.task, batch)
	if err == nil {
		return kept
	}
	if !retryable(err) {
		s.dropped(len(batch), false, err)
		return kept
	}
	s.mu.Lock()
	s.batch = append(batch, s.batch...)
	excess := max(len(s.batch)-pendingLimit, 0)
	s.batch = s.batch[excess:]
	s.mu.Unlock()
	s.dropped(excess, false, err)
	return err
}

// sendUsage reports usage, keeping it after a transient failure, which it returns.
func (s *eventStream) sendUsage(ctx context.Context, u multica.Usage) error {
	err := s.reporter.ReportUsage(ctx, s.task, u)
	switch {
	case retryable(err):
		s.mu.Lock()
		s.unreported = &u
		s.mu.Unlock()
		return err
	case err != nil:
		s.dropped(0, true, err)
	}
	return nil
}

// dropped logs transcript rows and usage Multica will not receive.
func (s *eventStream) dropped(rows int, usage bool, err error) {
	if (rows == 0 && !usage) || s.log == nil {
		return
	}
	s.log.Warn("attempt report dropped", "task", s.task, "messages", rows, "usage", usage, "error", err.Error())
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
	_ = s.sendUsage(ctx, u)
}
