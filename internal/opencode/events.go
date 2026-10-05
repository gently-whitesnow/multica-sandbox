package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type Reporter interface {
	ReportMessages(context.Context, string, []multica.Message) error
	ReportUsage(context.Context, string, multica.Usage) error
	ReportSession(context.Context, string, string) error
}
type eventStream struct {
	reporter      Reporter
	task, session string
	seq           int
	output        strings.Builder
	usage         multica.Usage
	redact        func(string) string
}
type nativeEvent struct {
	Type      string `json:"type"`
	Session   string `json:"sessionID"`
	Timestamp int64  `json:"timestamp"`
	Part      struct {
		ID        string `json:"id"`
		MessageID string `json:"messageID"`
		Session   string `json:"sessionID"`
		Text      string `json:"text"`
		Tool      string `json:"tool"`
		CallID    string `json:"callID"`
		Reason    string `json:"reason"`
		State     *struct {
			Status string          `json:"status"`
			Input  map[string]any  `json:"input"`
			Output json.RawMessage `json:"output"`
			Error  string          `json:"error"`
		} `json:"state"`
		Tokens *struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
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

// CLI JSON is an observation stream, never a second reasoning or retry loop.
func (s *eventStream) read(ctx context.Context, reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	seen := map[string]bool{}
	open, terminal, productive := false, false, false
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if s.redact != nil {
			line = s.redact(line)
		}
		var e nativeEvent
		if json.Unmarshal([]byte(line), &e) != nil || len(e.Session) > 256 || !strings.HasPrefix(e.Session, "ses_") {
			return fmt.Errorf("invalid native event")
		}
		if e.Type != "error" && (e.Part.ID == "" || len(e.Part.ID) > 256) {
			return fmt.Errorf("invalid native part identity")
		}
		if s.session == "" {
			s.session = e.Session
			if s.reporter != nil {
				if err := s.reporter.ReportSession(ctx, s.task, s.session); err != nil {
					return err
				}
			}
		}
		if e.Session != s.session || (e.Part.Session != "" && e.Part.Session != s.session) {
			return fmt.Errorf("native session mismatch")
		}
		key := e.Type + ":" + e.Part.ID
		if e.Part.ID != "" {
			if seen[key] {
				continue
			}
			if len(seen) >= 10000 {
				return fmt.Errorf("native event count exceeds limit")
			}
			seen[key] = true
		}
		when := time.Now().UTC()
		if e.Timestamp > 0 {
			when = time.UnixMilli(e.Timestamp).UTC()
		}
		msg := multica.Message{CreatedAt: when}
		messages := []multica.Message{}
		switch e.Type {
		case "step_start":
			open, terminal, productive = true, false, false
			msg.Type = "status"
			msg.Content = "running"
			messages = append(messages, msg)
		case "text", "reasoning":
			msg.Type = e.Type
			if e.Type == "reasoning" {
				msg.Type = "thinking"
			}
			msg.Content = e.Part.Text
			if len(msg.Content) > 65536 {
				return fmt.Errorf("native text exceeds report limit")
			}
			if msg.Content != "" {
				productive = true
				messages = append(messages, msg)
			}
			if e.Type == "text" {
				if s.output.Len()+len(msg.Content) > 65536 {
					return fmt.Errorf("native result exceeds report limit")
				}
				s.output.WriteString(msg.Content)
			}
		case "tool_use":
			if e.Part.State == nil || e.Part.CallID == "" || len(e.Part.CallID) > 256 || len(e.Part.Tool) > 256 {
				return fmt.Errorf("invalid native tool event")
			}
			state := e.Part.State
			if state.Status != "completed" && state.Status != "error" {
				return fmt.Errorf("nonterminal native tool event")
			}
			msg.Type = "tool_use"
			msg.Tool = e.Part.Tool
			msg.CallID = e.Part.CallID
			msg.Input = state.Input
			messages = append(messages, msg)
			result := msg
			result.Type = "tool_result"
			result.Input = nil
			if state.Status == "error" {
				result.Output = state.Error
			} else if len(state.Output) > 0 {
				if json.Unmarshal(state.Output, &result.Output) != nil {
					result.Output = string(state.Output)
				}
			}
			if len(result.Output) > 65536 {
				return fmt.Errorf("native tool result exceeds report limit")
			}
			messages = append(messages, result)
			productive = true
		case "step_finish":
			if !open {
				return fmt.Errorf("native finish without start")
			}
			if t := e.Part.Tokens; t != nil {
				fields := [][2]int64{{s.usage.Input, t.Input}, {s.usage.Output, t.Output}, {s.usage.Output, t.Reasoning}, {s.usage.CacheRead, t.Cache.Read}, {s.usage.CacheWrite, t.Cache.Write}}
				for _, f := range fields {
					if f[1] < 0 || f[0] > math.MaxInt64-f[1] {
						return fmt.Errorf("invalid native usage")
					}
				}
				if t.Output > math.MaxInt64-t.Reasoning || s.usage.Output > math.MaxInt64-t.Output-t.Reasoning {
					return fmt.Errorf("invalid native usage")
				}
				s.usage.Input += t.Input
				s.usage.Output += t.Output + t.Reasoning
				s.usage.CacheRead += t.Cache.Read
				s.usage.CacheWrite += t.Cache.Write
				productive = productive || (t.Input > 0 || t.Output > 0 || t.Reasoning > 0) || t.Cache.Read > 0 || t.Cache.Write > 0
				// CLI does not emit model IDs. Only attribute when trusted configuration applied one.
				if s.reporter != nil && s.usage.Model != "" {
					if err := s.reporter.ReportUsage(ctx, s.task, s.usage); err != nil {
						return err
					}
				}
			}
			open = false
			terminal = e.Part.Reason == "stop" && productive
		case "error":
			status := 0
			if e.Error.Name == "APIError" && e.Error.Data.Status >= 400 && e.Error.Data.Status <= 599 {
				status = e.Error.Data.Status
			}
			// Native error bodies/headers/messages can contain authorization; report numeric status only.
			return &execution.AgentFailure{Status: status}
		default:
			return fmt.Errorf("unsupported native event")
		}
		for i := range messages {
			s.seq++
			messages[i].Seq = s.seq
		}
		if len(messages) > 0 && s.reporter != nil {
			if err := s.reporter.ReportMessages(ctx, s.task, messages); err != nil {
				return err
			}
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("native event stream read failed")
	}
	if open || !terminal {
		return fmt.Errorf("native stream ended without completion")
	}
	return nil
}
