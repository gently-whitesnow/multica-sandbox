package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Upstream daemon values for OpenCode transcripts.
const (
	maxNativeLine     = 32 << 20
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

// eventStream maps `opencode run --format json` like the upstream OpenCode backend.
type eventStream struct {
	reporter Reporter
	task     string
	model    string
	// workDir names the retained workdir reported with the session.
	workDir string
	output  strings.Builder
	session string
	usage   multica.Usage
	active  atomic.Int64
	tools   atomic.Int64

	mu      sync.Mutex
	seq     int
	batch   []multica.Message
	pending string
}

type nativeEvent struct {
	Type      string `json:"type"`
	Session   string `json:"sessionID"`
	Timestamp int64  `json:"timestamp"`
	Part      struct {
		Text     string `json:"text"`
		Tool     string `json:"tool"`
		CallID   string `json:"callID"`
		Reason   string `json:"reason"`
		Metadata *struct {
			ProviderExecuted bool `json:"providerExecuted"`
		} `json:"metadata"`
		State *struct {
			Status string          `json:"status"`
			Input  map[string]any  `json:"input"`
			Output json.RawMessage `json:"output"`
			Error  string          `json:"error"`
		} `json:"state"`
		Cost   float64 `json:"cost"`
		Tokens *struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
			Total     int64 `json:"total"`
			Cache     struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Status int `json:"statusCode"`
		} `json:"data"`
	} `json:"error"`
}

var errorName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// read observes the CLI stream; it is never a second reasoning or retry loop.
// Unknown and non-JSON lines are skipped, as upstream does.
func (s *eventStream) read(ctx context.Context, reader io.Reader) error {
	s.touch()
	stop := s.flushEvery(reportInterval)
	defer stop()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxNativeLine)
	var failure error
	// Step bracketing is the only terminal signal in OpenCode's JSON stream.
	open, continuation, awaiting, finished, produced, void := false, false, false, false, false, false
	for scanner.Scan() {
		s.touch()
		var e nativeEvent
		line := strings.TrimSpace(scanner.Text())
		if line == "" || json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Session != "" && s.session == "" {
			s.mu.Lock()
			s.session, s.pending = e.Session, e.Session
			s.mu.Unlock()
		}
		when := time.Now().UTC()
		if e.Timestamp > 0 {
			when = time.UnixMilli(e.Timestamp).UTC()
		}
		switch e.Type {
		case "text":
			if e.Part.Text != "" {
				s.output.WriteString(e.Part.Text)
				s.add(multica.Message{Type: "text", Content: e.Part.Text, CreatedAt: when})
				produced = true
			}
		case "reasoning":
			if e.Part.Text != "" {
				s.add(multica.Message{Type: "thinking", Content: e.Part.Text, CreatedAt: when})
			}
		case "tool_use":
			s.tool(e, when)
			produced = true
			if e.Part.Metadata == nil || !e.Part.Metadata.ProviderExecuted {
				continuation = true
			}
		case "error":
			if failure == nil {
				failure = nativeFailure(e)
			}
		case "step_start":
			open, continuation, awaiting, produced = true, false, false, false
			s.add(multica.Message{Type: "status", Content: "running", CreatedAt: when})
		case "step_finish":
			open, finished = false, true
			awaiting = e.Part.Reason == "tool-calls" || (e.Part.Reason != "" && continuation)
			continuation = false
			if t := e.Part.Tokens; t != nil {
				// OpenCode 1.18 counts reasoning separately; Multica includes it in output.
				s.usage.Input += max(t.Input, 0)
				s.usage.Output += max(t.Output, 0) + max(t.Reasoning, 0)
				s.usage.CacheRead += max(t.Cache.Read, 0)
				s.usage.CacheWrite += max(t.Cache.Write, 0)
				produced = produced || t.Input > 0 || t.Output > 0 || t.Reasoning > 0 || t.Total > 0 || t.Cache.Read > 0 || t.Cache.Write > 0
			}
			produced = produced || e.Part.Cost > 0
			void = !produced
		}
	}
	s.reportUsage()
	// Upstream wording lets Multica classify a cut stream as retryable provider_network.
	switch {
	case failure != nil:
		return failure
	case scanner.Err() != nil:
		return &execution.AgentFailure{Message: "opencode stdout read error"}
	case open:
		return &execution.AgentFailure{Message: "opencode stream ended without a terminal signal (step still open at EOF)"}
	case awaiting:
		return &execution.AgentFailure{Message: "opencode stream ended without a terminal signal (last step required a continuation that never started)"}
	case void:
		return &execution.AgentFailure{Message: "opencode stream ended on an empty step (no text, no tool call, no reported usage) — the provider produced nothing"}
	case !finished:
		// Stricter than upstream: exit 0 without any finished step is not a completion.
		return &execution.AgentFailure{Message: "opencode stream ended without a terminal signal (no step finished)"}
	}
	return nil
}

func (s *eventStream) tool(e nativeEvent, when time.Time) {
	s.tools.Add(1)
	call := multica.Message{Type: "tool_use", Tool: e.Part.Tool, CallID: e.Part.CallID, CreatedAt: when}
	state := e.Part.State
	if state == nil {
		s.add(call)
		return
	}
	call.Input = state.Input
	if state.Status != "completed" && state.Status != "error" {
		s.add(call)
		return
	}
	result := multica.Message{Type: "tool_result", Tool: e.Part.Tool, CallID: e.Part.CallID, CreatedAt: when}
	output := ""
	if state.Status == "error" && state.Error != "" {
		output = state.Error
	} else if len(state.Output) > 0 && json.Unmarshal(state.Output, &output) != nil {
		output = string(state.Output)
	}
	truncated := len(output) > toolOutputPreview
	if truncated {
		end := toolOutputPreview
		for !utf8.RuneStart(output[end]) {
			end--
		}
		output = output[:end]
	}
	result.Output, result.OutputTruncated = output, &truncated
	s.add(call, result)
}

// Native error bodies and headers can carry authorization; report status and error name only.
func nativeFailure(e nativeEvent) error {
	status := 0
	if e.Error.Name == "APIError" && e.Error.Data.Status >= 400 && e.Error.Data.Status <= 599 {
		status = e.Error.Data.Status
	}
	failure := &execution.AgentFailure{Status: status}
	if status == 0 && errorName.MatchString(e.Error.Name) {
		failure.Message = "opencode reported " + e.Error.Name
	}
	return failure
}

func (s *eventStream) touch() { s.active.Store(time.Now().UnixNano()) }

// idle reports silence longer than the upstream OpenCode idle watchdog.
func (s *eventStream) idle(window time.Duration) bool {
	return time.Since(time.Unix(0, s.active.Load())) > window
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

// reportUsage sends the cumulative total once; OpenCode emits no model, so attribution is the applied model.
func (s *eventStream) reportUsage() {
	u := s.usage
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
