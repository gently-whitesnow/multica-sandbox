package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type httpEvent struct {
	ID, Type string
	Created  int64
	Data     struct {
		SessionID, ParentID, AssistantMessageID, InboxID string
		ID, Name, Delta, Text, Finish                    string
		Ordinal                                          int
		Input                                            toolInput
		Content, Error                                   json.RawMessage
		Tokens                                           *struct {
			Input, Output, Reasoning int64
			Cache                    struct{ Read, Write int64 }
		}
		Metadata json.RawMessage
		Shell    struct{ ID string }
		Form     struct{ ID, SessionID string }
	}
}

func (e *httpEvent) UnmarshalJSON(raw []byte) error {
	var envelope struct {
		ID, Type string
		Created  int64
		Data     json.RawMessage
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	e.ID, e.Type, e.Created = envelope.ID, envelope.Type, envelope.Created
	if !nativeHTTPEventType(e.Type) {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &fields); err != nil {
		return err
	}
	if e.Type != "session.tool.success" {
		delete(fields, "metadata")
	}
	data, _ := json.Marshal(fields)
	return json.Unmarshal(data, &e.Data)
}

func nativeHTTPEventType(kind string) bool {
	switch kind {
	case "session.created", "session.inbox.delivered", "session.inbox.cancelled", "session.execution.started", "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted",
		"session.step.started", "session.step.ended", "session.step.failed", "session.shell.started", "session.shell.ended",
		"session.text.delta", "session.text.ended", "session.reasoning.delta", "session.reasoning.ended",
		"session.tool.input.started", "session.tool.called", "session.tool.progress", "session.tool.success", "session.tool.failed", "permission.asked", "form.created",
		"session.retry.scheduled", "session.compaction.started", "session.compaction.delta", "session.compaction.ended", "session.compaction.failed":
		return true
	}
	return false
}

type httpCall struct {
	name, id string
	running  bool
	finished bool
}

type httpPromotion struct {
	step, execution int64
	processed       bool
	verified        bool
	verifyAfter     time.Time
}

type httpText struct {
	size   int
	digest hash.Hash
}

type httpTurn struct {
	r                        *running
	session                  string
	mu                       sync.Mutex
	connected                chan struct{}
	changed                  chan struct{}
	ready                    bool
	started, busy, terminal  bool
	execution, step          int64
	stepAt                   int64
	terminalAt, shellEndedAt int64
	failure                  error
	seen                     map[string]bool
	seenOrder                []string
	owned                    map[string]bool
	calls                    map[string]*httpCall
	doneCalls                map[string]bool
	doneCallOrder            []string
	texts                    map[string]*httpText
	doneTexts                map[string]bool
	doneTextOrder            []string
	shells                   map[string]bool
	delivered                map[string]httpPromotion
	cancelled                map[string]bool
	unsafeResume             bool
	fatal                    bool
}

func newHTTPTurn(r *running, session string) *httpTurn {
	return &httpTurn{r: r, session: session, connected: make(chan struct{}), changed: make(chan struct{}, 1),
		seen: map[string]bool{}, owned: map[string]bool{session: true}, calls: map[string]*httpCall{},
		texts: map[string]*httpText{}, shells: map[string]bool{}, delivered: map[string]httpPromotion{}, cancelled: map[string]bool{}, doneCalls: map[string]bool{}, doneTexts: map[string]bool{}}
}

func (h *httpTurn) signal() {
	select {
	case h.changed <- struct{}{}:
	default:
	}
}

func (h *httpTurn) consume(event httpEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fatal {
		return
	}
	if strings.HasPrefix(event.Type, "job.") || event.Type == "session.execution.started" && !h.owned[event.Data.SessionID] {
		h.unsafeResume = true
		h.r.nativeClean = false
	}
	if event.Type == "server.connected" {
		if !h.ready {
			h.ready = true
			close(h.connected)
		}
		return
	}
	if event.Type == "session.created" && h.owned[event.Data.ParentID] && sessionID.MatchString(event.Data.SessionID) {
		h.owned[event.Data.SessionID] = true
	}
	if !h.owned[event.Data.SessionID] || event.ID != "" && h.seen[event.ID] {
		return
	}
	h.r.nativeClean = false
	if len(h.calls) > 4096 || len(h.texts) > 4096 || len(h.owned) > 4096 || len(h.delivered) > 8192 || len(h.cancelled) > 8192 {
		h.fatal = true
		h.failure = &execution.HostFailure{Message: "Sandbox bounded native HTTP event state was exhausted"}
		h.signal()
		return
	}
	if event.ID != "" {
		h.seen[event.ID] = true
		h.seenOrder = append(h.seenOrder, event.ID)
		if len(h.seenOrder) > 16384 {
			delete(h.seen, h.seenOrder[0])
			h.seenOrder = h.seenOrder[1:]
		}
	}
	h.r.events.touch()
	h.textEvent(event)
	h.toolEvent(event)
	h.lifecycleEvent(event)
	h.signal()
}

func (h *httpTurn) textEvent(e httpEvent) {
	if e.Data.SessionID != h.session {
		return
	}
	kind := ""
	if strings.HasPrefix(e.Type, "session.text.") {
		kind = "text"
	}
	if strings.HasPrefix(e.Type, "session.reasoning.") {
		kind = "thinking"
	}
	if kind == "" {
		return
	}
	key := e.Data.SessionID + ":" + e.Data.AssistantMessageID + ":" + strconv.Itoa(e.Data.Ordinal) + ":" + kind
	if h.doneTexts[key] {
		return
	}
	prior := h.texts[key]
	if prior == nil {
		prior = &httpText{digest: sha256.New()}
		h.texts[key] = prior
	}
	switch {
	case strings.HasSuffix(e.Type, ".delta"):
		prior.size += len(e.Data.Delta)
		_, _ = prior.digest.Write([]byte(e.Data.Delta))
		h.r.events.chunk(kind, e.Data.Delta)
	case strings.HasSuffix(e.Type, ".ended"):
		if len(e.Data.Text) >= prior.size {
			prefix := sha256.Sum256([]byte(e.Data.Text[:prior.size]))
			if bytes.Equal(prefix[:], prior.digest.Sum(nil)) {
				h.r.events.chunk(kind, e.Data.Text[prior.size:])
			}
		}
		delete(h.texts, key)
		h.doneTexts[key] = true
		h.doneTextOrder = append(h.doneTextOrder, key)
		if len(h.doneTextOrder) > 1024 {
			delete(h.doneTexts, h.doneTextOrder[0])
			h.doneTextOrder = h.doneTextOrder[1:]
		}
	}
}

func (h *httpTurn) toolEvent(e httpEvent) {
	if !strings.HasPrefix(e.Type, "session.tool.") {
		return
	}
	key := e.Data.SessionID + ":" + e.Data.AssistantMessageID + ":" + e.Data.ID
	if h.doneCalls[key] {
		return
	}
	call := h.calls[key]
	if call == nil {
		id := e.Data.AssistantMessageID + ":" + e.Data.ID
		if e.Data.SessionID != h.session {
			id = e.Data.SessionID + ":" + id
		}
		call = &httpCall{id: id, name: "tool"}
		h.calls[key] = call
	}
	if e.Data.Name != "" {
		call.name = e.Data.Name
	}
	switch e.Type {
	case "session.tool.called":
		if call.running || call.finished {
			return
		}
		call.running = true
		h.r.events.running.Add(1)
		h.r.events.tools.Add(1)
		h.r.events.add(multica.Message{Type: "tool_use", Tool: call.name, CallID: call.id, Input: e.Data.Input, CreatedAt: time.Now().UTC()})
	case "session.tool.success", "session.tool.failed":
		if call.finished {
			return
		}
		call.finished = true
		if call.running {
			call.running = false
			h.r.events.running.Add(-1)
		}
		output, truncated := preview(nativeOutput(e.Data.Content, e.Data.Error))
		h.r.events.add(multica.Message{Type: "tool_result", Tool: call.name, CallID: call.id, Output: output, OutputTruncated: &truncated, CreatedAt: time.Now().UTC()})
		var metadata shellMetadata
		_ = json.Unmarshal(e.Data.Metadata, &metadata)
		if shell := metadata.background(); shell != "" && e.Data.SessionID == h.session {
			h.unsafeResume = true
			h.shell(shell, true)
		}
		if metadata.Status == "running" {
			h.unsafeResume = true
		}
		delete(h.calls, key)
		h.doneCalls[key] = true
		h.doneCallOrder = append(h.doneCallOrder, key)
		if len(h.doneCallOrder) > 1024 {
			delete(h.doneCalls, h.doneCallOrder[0])
			h.doneCallOrder = h.doneCallOrder[1:]
		}
	}
}

func (h *httpTurn) shell(id string, running bool) {
	if id == "" || h.shells[id] == running {
		return
	}
	if running {
		h.shells[id] = true
		h.r.events.background.Add(1)
	} else {
		delete(h.shells, id)
		h.r.events.background.Add(-1)
	}
}

func (h *httpTurn) lifecycleEvent(e httpEvent) {
	if e.Data.SessionID != h.session {
		return
	}
	if e.Type == "session.shell.started" {
		h.shell(e.Data.Shell.ID, true)
	}
	if e.Type == "session.shell.ended" {
		h.shell(e.Data.Shell.ID, false)
		h.shellEndedAt = max(h.shellEndedAt, e.Created)
	}
	switch e.Type {
	case "session.inbox.delivered":
		if _, known := h.delivered[e.Data.InboxID]; !known {
			h.delivered[e.Data.InboxID] = httpPromotion{step: h.step, execution: h.execution}
		}
	case "session.inbox.cancelled":
		h.cancelled[e.Data.InboxID] = true
	case "session.execution.started":
		h.started, h.busy, h.terminal = true, true, false
		h.execution++
	case "session.step.started":
		h.step++
		h.stepAt = max(h.stepAt, e.Created)
		for id, promotion := range h.delivered {
			if promotion.execution == h.execution && h.step > promotion.step {
				promotion.processed = true
				h.delivered[id] = promotion
			}
		}
	case "session.step.ended", "session.step.failed":
		if t := e.Data.Tokens; t != nil {
			h.r.spent.usage = addUsage(h.r.spent.usage, multica.Usage{Input: t.Input, Output: t.Output + t.Reasoning, CacheRead: t.Cache.Read, CacheWrite: t.Cache.Write})
		}
		if e.Data.Finish == "length" || e.Data.Finish == "content-filter" {
			h.failure = &execution.AgentFailure{Message: "OpenCode stopped before completing its response"}
		}
		if e.Type == "session.step.failed" {
			h.failure = nativeHTTPFailure(e.Data.Error)
		}
	case "session.execution.succeeded":
		h.busy, h.terminal = false, true
		h.terminalAt = e.Created
	case "session.execution.failed", "session.execution.interrupted":
		h.busy, h.terminal = false, true
		h.terminalAt = e.Created
		if h.failure == nil {
			h.failure = nativeHTTPFailure(e.Data.Error)
		}
	}
}

func nativeHTTPFailure(raw json.RawMessage) error {
	var failure struct {
		Type   string
		Status int
	}
	_ = json.Unmarshal(raw, &failure)
	if failure.Status >= 400 && failure.Status <= 599 {
		return &execution.AgentFailure{Status: failure.Status}
	}
	if errorName.MatchString(failure.Type) {
		return &execution.AgentFailure{Message: "OpenCode reported " + failure.Type}
	}
	return &execution.AgentFailure{Message: "OpenCode native execution did not complete"}
}

func (h *httpTurn) status() (started, busy, terminal bool, execution, stepAt int64, shells int, failure error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started, h.busy, h.terminal && h.terminalAt >= h.shellEndedAt, h.execution, h.stepAt, len(h.shells), h.failure
}

// readHTTPEvents consumes a single volatile stream; disconnect fails closed rather
// than silently continuing after missed input, tool or terminal events.
func readHTTPEvents(ctx context.Context, stdout io.Reader, consume func(httpEvent)) error {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(nil, 4<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				var event httpEvent
				if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
					var mismatch *json.UnmarshalTypeError
					if errors.As(err, &mismatch) {
						return fmt.Errorf("invalid native event field %s", mismatch.Field)
					}
					return fmt.Errorf("invalid native event encoding")
				}
				consume(event)
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			if data.Len() > 4<<20 {
				return fmt.Errorf("native event frame exceeded its bound")
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return streamCut
}
