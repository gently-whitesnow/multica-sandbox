package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// OpenCode 2.x ends a turn while shells it moved to the background still run, then
// resumes the session itself when they finish, outside any ACP prompt. The adapter keeps
// the agent process, which owns those shells, and reads the session through a standalone
// server on the same database until they finished and the resumed turns went idle.
var (
	followInterval = 10 * time.Second
	// followPage bounds one session read; followPages bounds a scan.
	followPage, followPages = 50, 40
	// followFailures consecutive unreadable reads fail the attempt.
	followFailures = 6
)

// errFollow is the sandbox's failure, not the agent's: the retry resumes the session.
var errFollow = &execution.HostFailure{Message: "Sandbox could not read the OpenCode session while background shells ran; Multica retries the task"}

// shellMetadata marks a shell call OpenCode moved to the background: status running.
type shellMetadata struct {
	Status  string `json:"status"`
	ShellID string `json:"shellID"`
}

func (m shellMetadata) background() string {
	if m.Status != "running" {
		return ""
	}
	return m.ShellID
}

// backgroundShell reads the ACP rawOutput of a completed shell call.
func backgroundShell(raw json.RawMessage) string {
	var out struct {
		Metadata shellMetadata `json:"metadata"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return ""
	}
	return out.Metadata.background()
}

// shell records a background shell of the followed session as running or finished.
// The caller holds c.mu.
func (c *acpClient) shell(id string, running bool) {
	if id == "" || c.shells[id] == running {
		return
	}
	if running {
		c.shells[id] = true
		c.events.background.Add(1)
		return
	}
	if c.shells[id] {
		delete(c.shells, id)
		c.events.background.Add(-1)
	}
}

func (c *acpClient) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.shells)
}

// toolInput is a tool call's input object. OpenCode can report a call's input as a
// string before it settles; that reads as no input instead of failing the whole read.
type toolInput map[string]any

func (i *toolInput) UnmarshalJSON(data []byte) error {
	var input map[string]any
	if json.Unmarshal(data, &input) != nil {
		input = nil
	}
	*i = input
	return nil
}

// nativeMessage is one row of the session message API.
type nativeMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Metadata struct {
		Source  string `json:"source"`
		ShellID string `json:"shellID"`
	} `json:"metadata"`
	Content []nativePart `json:"content"`
	Tokens  *struct {
		Input, Output, Reasoning int64
		Cache                    struct{ Read, Write int64 }
	} `json:"tokens"`
}

type nativePart struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Text  string `json:"text"`
	State *struct {
		Status   string          `json:"status"`
		Input    toolInput       `json:"input"`
		Content  json.RawMessage `json:"content"`
		Error    json.RawMessage `json:"error"`
		Metadata shellMetadata   `json:"metadata"`
	} `json:"state"`
}

// follow waits for the background shells a finished turn left running and reports the
// turns OpenCode resumed for them; it returns their usage. Messages created before the
// turn ended reached Multica over ACP and only settle shells.
func (r *running) follow(ctx context.Context, client *acpClient, id acp.SessionId, since, ended time.Time) (multica.Usage, error) {
	client.mu.Lock()
	client.following = true
	client.mu.Unlock()
	var usage multica.Usage
	seen := map[string]bool{}
	floor, woke, idle := since.UnixMilli(), int64(0), int64(0)
	// tool marks a resumed step whose tool still runs: it counts as a running tool.
	tool := false
	defer func() { r.events.toolRunning(&tool, false) }()
	ticker := time.NewTicker(followInterval)
	defer ticker.Stop()
	for failures := 0; ; {
		select {
		case <-ctx.Done():
			return usage, ctx.Err()
		case <-ticker.C:
		}
		messages, err := r.nativeMessages(ctx, id, floor)
		if err != nil {
			if ctx.Err() != nil {
				return usage, ctx.Err()
			}
			if failures++; failures >= followFailures {
				return usage, r.unreadable(err)
			}
			continue
		}
		failures, floor = 0, ended.UnixMilli()
		busy := false
	scan:
		for _, m := range messages {
			if seen[m.ID] {
				continue
			}
			after := m.Time.Created >= ended.UnixMilli()
			switch {
			case m.Type == "assistant" && after && (m.Time.Completed == 0 || m.toolRunning()):
				// A resumed step still streams or runs a tool; it and what follows are read next time.
				busy = m.Time.Completed != 0
				break scan
			case m.Type == "synthetic" && m.Metadata.Source == "shell":
				client.mu.Lock()
				client.shell(m.Metadata.ShellID, false)
				client.mu.Unlock()
				if after {
					woke = max(woke, m.Time.Created)
				}
			case !after:
			case m.Type == "assistant":
				usage = addUsage(usage, r.events.native(client, m))
				woke = max(woke, m.Time.Created)
			case m.Type == "idle":
				idle = max(idle, m.Time.Created)
			}
			if after {
				r.events.touch()
			}
			seen[m.ID] = true
		}
		r.events.toolRunning(&tool, busy)
		if client.waiting() == 0 && (woke == 0 || idle > woke) {
			return usage, nil
		}
	}
}

// unreadable ends a follow whose reads kept failing. An attempt out of memory fails as
// such; otherwise the cause goes to the controller log and the attempt fails as errFollow.
func (r *running) unreadable(err error) error {
	var oom *execution.OOMError
	var stall *execution.MemoryStallError
	if errors.As(err, &oom) || errors.As(err, &stall) {
		return err
	}
	if r.adapter != nil && r.adapter.Log != nil {
		r.adapter.Log.Warn("opencode session unreadable", "task", r.task.ID, "attempt", r.task.AttemptKey(), "reads", followFailures, "error", redact(err.Error(), r.secrets()))
	}
	return errFollow
}

func (m nativeMessage) toolRunning() bool {
	return slices.ContainsFunc(m.Content, func(p nativePart) bool {
		return p.Type == "tool" && p.State != nil && p.State.Status != "completed" && p.State.Status != "error"
	})
}

// toolRunning counts a resumed tool as running while on.
func (s *eventStream) toolRunning(state *bool, on bool) {
	switch {
	case on && !*state:
		s.running.Add(1)
	case !on && *state:
		s.running.Add(-1)
	}
	*state = on
}

// nativeMessages reads the session newest first down to floor and returns the messages
// created at or after it, oldest first.
func (r *running) nativeMessages(ctx context.Context, id acp.SessionId, floor int64) ([]nativeMessage, error) {
	var all []nativeMessage
	cursor := ""
	for range followPages {
		query := url.Values{"order": {"desc"}, "limit": {strconv.Itoa(followPage)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Data   []nativeMessage `json:"data"`
			Cursor struct {
				Next string `json:"next"`
			} `json:"cursor"`
		}
		path := "/api/session/" + string(id) + "/message?" + query.Encode()
		var stderr stderrTail
		err := r.stream(ctx, []string{"opencode", "api", "--standalone", "GET", path}, &stderr, func(_ io.Writer, stdout io.Reader) error {
			return json.NewDecoder(stdout).Decode(&page)
		})
		if err != nil {
			if tail := stderr.String(); tail != "" {
				err = fmt.Errorf("%w; stderr: %s", err, tail)
			}
			return nil, err
		}
		older := false
		for _, m := range page.Data {
			if m.Time.Created < floor {
				older = true
				break
			}
			all = append(all, m)
		}
		if older || len(page.Data) < followPage || page.Cursor.Next == "" {
			slices.Reverse(all)
			return all, nil
		}
		cursor = page.Cursor.Next
	}
	return nil, errFollow
}

// native reports a settled resumed assistant message like its ACP stream and returns its usage.
func (s *eventStream) native(client *acpClient, m nativeMessage) multica.Usage {
	now := time.Now().UTC()
	for _, part := range m.Content {
		switch part.Type {
		case "text":
			s.chunk("text", part.Text)
		case "reasoning":
			s.chunk("thinking", part.Text)
		case "tool":
			if part.State == nil {
				continue
			}
			s.tools.Add(1)
			s.add(multica.Message{Type: "tool_use", Tool: part.Name, CallID: part.ID, Input: part.State.Input, CreatedAt: now})
			output, truncated := preview(nativeOutput(part.State.Content, part.State.Error))
			s.add(multica.Message{Type: "tool_result", Tool: part.Name, CallID: part.ID, Output: output, OutputTruncated: &truncated, CreatedAt: now})
			client.mu.Lock()
			client.shell(part.State.Metadata.background(), true)
			client.mu.Unlock()
		}
	}
	return m.usage()
}

func (m nativeMessage) usage() multica.Usage {
	if m.Tokens == nil {
		return multica.Usage{}
	}
	return multica.Usage{Input: m.Tokens.Input, Output: m.Tokens.Output + m.Tokens.Reasoning, CacheRead: m.Tokens.Cache.Read, CacheWrite: m.Tokens.Cache.Write}
}

// nativeOutput joins a tool state's text content, else its error message.
func nativeOutput(content, failure json.RawMessage) string {
	var blocks []struct{ Type, Text string }
	text := ""
	if json.Unmarshal(content, &blocks) == nil {
		for _, block := range blocks {
			if block.Type == "text" {
				text += block.Text
			}
		}
	}
	if text != "" || len(failure) == 0 {
		return text
	}
	var plain string
	var structured struct{ Message string }
	switch {
	case json.Unmarshal(failure, &plain) == nil:
		return plain
	case json.Unmarshal(failure, &structured) == nil:
		return structured.Message
	}
	return ""
}

func addUsage(a, b multica.Usage) multica.Usage {
	a.Input, a.Output, a.CacheRead, a.CacheWrite = a.Input+b.Input, a.Output+b.Output, a.CacheRead+b.CacheRead, a.CacheWrite+b.CacheWrite
	return a
}
