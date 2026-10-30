package opencode

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func init() { mcpSettle = 10 * time.Millisecond }

// fakeAgent speaks the agent side of ACP like `opencode acp`, one JSON-RPC message per line.
type fakeAgent struct {
	version string
	// updates are sent, in order, before the prompt is answered.
	updates []map[string]any
	// stop answers the prompt; empty waits for session/cancel and answers cancelled.
	stop  string
	usage map[string]any
	// loadFails refuses session/load; hangup exits without answering the prompt.
	loadFails, hangup bool
	// fail answers the prompt with this JSON-RPC error data and a provider body.
	fail map[string]any
	// failCode is the JSON-RPC code of fail (default -32603).
	failCode int
	// prompted receives the prompt text once it arrives.
	prompted chan string

	mu      sync.Mutex
	methods []string
	params  map[string]json.RawMessage
	at      map[string]time.Time
}

type rpc struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (f *fakeAgent) serve(out io.WriteCloser, in io.Reader) {
	defer out.Close()
	var write sync.Mutex
	send := func(message map[string]any) {
		message["jsonrpc"] = "2.0"
		data, _ := json.Marshal(message)
		write.Lock()
		defer write.Unlock()
		_, _ = out.Write(append(data, '\n'))
	}
	reply := func(id json.RawMessage, result any) { send(map[string]any{"id": id, "result": result}) }
	update := func(session string, u map[string]any) {
		send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": session, "update": u}})
	}
	cancelled := make(chan struct{})
	var once sync.Once
	scanner := bufio.NewScanner(in)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var m rpc
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, m.Method)
		if f.params == nil {
			f.params, f.at = map[string]json.RawMessage{}, map[string]time.Time{}
		}
		f.params[m.Method], f.at[m.Method] = m.Params, time.Now()
		f.mu.Unlock()
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "OpenCode", "version": f.version}, "agentCapabilities": map[string]any{"loadSession": true, "mcpCapabilities": map[string]any{"http": true}}})
		case "session/new":
			reply(m.ID, map[string]any{"sessionId": "ses_fresh"})
		case "session/load":
			if f.loadFails {
				send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32602, "message": "session not found"}})
				continue
			}
			update("ses_prior", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "replayed history"}})
			reply(m.ID, map[string]any{})
		case "session/cancel":
			once.Do(func() { close(cancelled) })
		case "session/prompt":
			var p struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct{ Text string }
			}
			_ = json.Unmarshal(m.Params, &p)
			if f.prompted != nil {
				f.prompted <- p.Prompt[0].Text
			}
			go func(id json.RawMessage) {
				for _, u := range f.updates {
					session, child := u["_session"].(string)
					if !child {
						session = p.SessionID
					}
					plain := map[string]any{}
					for k, v := range u {
						if k != "_session" {
							plain[k] = v
						}
					}
					update(session, plain)
				}
				switch {
				case f.hangup:
					out.Close()
				case f.fail != nil:
					send(map[string]any{"id": id, "error": map[string]any{"code": cmp.Or(f.failCode, -32603), "message": "Internal error: secret-body", "data": f.fail}})
				case f.stop == "":
					<-cancelled
					reply(id, map[string]any{"stopReason": "cancelled"})
				default:
					reply(id, map[string]any{"stopReason": f.stop, "usage": f.usage})
				}
			}(m.ID)
		}
	}
}

func (f *fakeAgent) called(method string) (json.RawMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	params, ok := f.params[method]
	return params, ok
}

// turnWith runs one adapter turn against the fake agent over in-memory pipes.
func turnWith(t *testing.T, ctx context.Context, agent *fakeAgent, r *running) error {
	t.Helper()
	if r.events == nil {
		r.events = &eventStream{}
	}
	if r.adapter == nil {
		r.adapter = &Adapter{}
	}
	if r.workload == nil {
		r.workload = &stubWorkload{}
	}
	if r.prompt == nil {
		r.prompt = []byte("do the task")
	}
	if r.task.Agent == nil {
		r.task = safeTask()
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); agent.serve(outW, inR) }()
	err := r.converse(ctx, inW, outR)
	_ = inW.Close()
	<-done
	return err
}

func toolUpdates() []map[string]any {
	return []map[string]any{
		{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "think"}},
		{"sessionUpdate": "tool_call", "toolCallId": "call-1", "title": "bash", "kind": "execute", "status": "pending", "rawInput": map[string]any{}},
		{"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "title": "make build", "status": "in_progress", "rawInput": map[string]any{"command": "make build"}},
		{"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "title": "make build", "status": "completed", "content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "built"}}}},
		{"sessionUpdate": "tool_call", "_session": "ses_child", "toolCallId": "call-2", "title": "read", "status": "completed", "rawInput": map[string]any{"path": "a"}, "rawOutput": map[string]any{"output": "child output"}},
		{"sessionUpdate": "agent_message_chunk", "_session": "ses_child", "content": map[string]any{"type": "text", "text": "child text"}},
		{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "ans"}},
		{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "wer"}},
	}
}

func TestACPTurnReportsMessagesUsageAndSession(t *testing.T) {
	reporter := &reportStub{}
	agent := &fakeAgent{version: "2.2.2", updates: toolUpdates(), stop: "end_turn", usage: map[string]any{"inputTokens": 30, "outputTokens": 9, "thoughtTokens": 3, "cachedReadTokens": 4, "cachedWriteTokens": 5, "totalTokens": 51}}
	r := &running{events: &eventStream{reporter: reporter, model: "managed-inference/selected"}}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	output, session, _ := r.events.result()
	if output != "answer" || session != "ses_fresh" || len(reporter.sessions) == 0 || reporter.sessions[0] != "ses_fresh" {
		t.Fatalf("result: %q %q %v", output, session, reporter.sessions)
	}
	var kinds []string
	for i, m := range reporter.messages {
		if m.Seq != i+2 {
			t.Fatalf("sequence: %+v", reporter.messages)
		}
		kinds = append(kinds, m.Type)
	}
	if strings.Join(kinds, ",") != "status,thinking,tool_use,tool_result,tool_use,tool_result,text" {
		t.Fatalf("messages: %v", kinds)
	}
	use, result := reporter.messages[2], reporter.messages[3]
	if use.Tool != "bash" || use.CallID != "call-1" || use.Input["command"] != "make build" || result.Output != "built" || result.CallID != "call-1" || *result.OutputTruncated {
		t.Fatalf("tool call keyed by toolCallId: %+v %+v", use, result)
	}
	if child := reporter.messages[5]; child.Tool != "read" || child.Output != "child output" || strings.Contains(output, "child") {
		t.Fatalf("child session: %+v", child)
	}
	want := multica.Usage{Provider: "opencode", Model: "managed-inference/selected", Input: 30, Output: 12, CacheRead: 4, CacheWrite: 5}
	if len(reporter.usage) != 1 || reporter.usage[0] != want {
		t.Fatalf("usage: %+v", reporter.usage)
	}
	params, _ := agent.called("initialize")
	var init struct {
		ClientCapabilities struct {
			Fs       map[string]bool
			Terminal bool
		}
	}
	if json.Unmarshal(params, &init) != nil || init.ClientCapabilities.Terminal || init.ClientCapabilities.Fs["readTextFile"] || init.ClientCapabilities.Fs["writeTextFile"] {
		t.Fatalf("client offered filesystem or terminal: %s", params)
	}
}

func TestACPVersionOneUsageComesFromExport(t *testing.T) {
	export := `banner {"messages":[` +
		`{"info":{"role":"assistant","time":{"created":1},"tokens":{"input":100,"output":100}}},` +
		`{"info":{"role":"user","time":{"created":9999999999999}}},` +
		`{"info":{"role":"assistant","time":{"created":9999999999999},"tokens":{"input":10,"output":2,"reasoning":3,"cache":{"read":4,"write":5}}}},` +
		`{"info":{"role":"assistant","time":{"created":9999999999999},"tokens":{"input":20,"output":7}}}]}`
	for name, c := range map[string]struct {
		export string
		want   multica.Usage
	}{
		"export":    {export, multica.Usage{Input: 30, Output: 12, CacheRead: 4, CacheWrite: 5}},
		"truncated": {export[:40], multica.Usage{Input: 20, Output: 7}},
	} {
		t.Run(name, func(t *testing.T) {
			workload := &stubWorkload{capture: []byte(c.export)}
			agent := &fakeAgent{version: "1.18.35", updates: toolUpdates(), stop: "end_turn", usage: map[string]any{"inputTokens": 20, "outputTokens": 7, "totalTokens": 27}}
			r := &running{workload: workload}
			if err := turnWith(t, context.Background(), agent, r); err != nil {
				t.Fatal(err)
			}
			if _, _, u := r.events.result(); u != c.want || strings.Join(workload.captured, " ") != "opencode export ses_fresh" {
				t.Fatalf("usage %+v from %v", u, workload.captured)
			}
		})
	}
}

func TestACPStopReasonsDecideOutcome(t *testing.T) {
	text := []map[string]any{{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "done"}}}
	for name, c := range map[string]struct {
		agent   *fakeAgent
		failure string
	}{
		"end turn":   {&fakeAgent{updates: text, stop: "end_turn"}, ""},
		"usage only": {&fakeAgent{stop: "end_turn", usage: map[string]any{"inputTokens": 1, "outputTokens": 0, "totalTokens": 1}}, ""},
		"nothing":    {&fakeAgent{stop: "end_turn"}, "the provider produced nothing"},
		"max tokens": {&fakeAgent{updates: text, stop: "max_tokens"}, "output length limit"},
		"refusal":    {&fakeAgent{updates: text, stop: "refusal"}, "refused the request"},
		"cancelled":  {&fakeAgent{updates: text, stop: "cancelled"}, "cancelled the turn"},
		"other":      {&fakeAgent{updates: text, stop: "max_turn_requests"}, "opencode stopped: max_turn_requests"},
		"hangup":     {&fakeAgent{updates: text, hangup: true}, "opencode stream ended without a terminal signal"},
		"rate limit": {&fakeAgent{fail: map[string]any{"errorName": "provider.rate-limit", "service": "session"}}, "HTTP 429"},
		"auth":       {&fakeAgent{fail: map[string]any{}, failCode: -32000}, "HTTP 401"},
		"named":      {&fakeAgent{fail: map[string]any{"errorName": "APIError", "service": "session"}}, "opencode reported APIError"},
		"unnamed":    {&fakeAgent{fail: map[string]any{"errorName": "secret body", "authorization": "Bearer secret-body"}}, "JSON-RPC -32603"},
	} {
		t.Run(name, func(t *testing.T) {
			c.agent.version = "2.2.2"
			err := turnWith(t, context.Background(), c.agent, &running{})
			if c.failure == "" && err != nil || c.failure != "" && (err == nil || !strings.Contains(err.Error(), c.failure)) {
				t.Fatalf("error=%v", err)
			}
			var failure *execution.AgentFailure
			if err != nil && (!errors.As(err, &failure) || strings.Contains(err.Error(), "secret")) {
				t.Fatalf("unsafe or unclassified failure: %v", err)
			}
		})
	}
}

func TestACPProviderErrorTakesTheRelayedStatus(t *testing.T) {
	grants := relay.NewGrants(inference.RelayPrefix)
	r := &running{task: safeTask(), adapter: &Adapter{InferenceRelay: grants}}
	var err error
	if r.inferenceToken, err = grants.Issue(r.task.AttemptKey(), relay.Upstream{Origin: "https://gateway.example.invalid", Credential: "sk-workspace"}); err != nil {
		t.Fatal(err)
	}
	for status, agent := range map[int]*fakeAgent{
		422: {version: "2.2.2", fail: map[string]any{"errorName": "provider.invalid-request"}},
		403: {version: "2.2.2", fail: map[string]any{}, failCode: -32000},
	} {
		grants.Responded(r.task.AttemptKey(), status)
		if err := turnWith(t, context.Background(), agent, r); err == nil || err.Error() != fmt.Sprintf("Agent inference request failed (HTTP %d)", status) {
			t.Fatalf("gateway refusal status lost: %v", err)
		}
	}
}

func TestACPCancellationSendsSessionCancel(t *testing.T) {
	agent := &fakeAgent{version: "2.2.2", prompted: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-agent.prompted; cancel() }()
	start := time.Now()
	if err := turnWith(t, ctx, agent, &running{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn: %v", err)
	}
	if _, ok := agent.called("session/cancel"); !ok || time.Since(start) > cancelGrace {
		t.Fatal("session/cancel not sent before teardown")
	}
}

func TestACPResumeLoadsAndIgnoresReplay(t *testing.T) {
	reporter := &reportStub{}
	agent := &fakeAgent{version: "2.2.2", updates: []map[string]any{{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "new"}}}, stop: "end_turn"}
	r := &running{resume: "ses_prior", events: &eventStream{reporter: reporter}}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	output, session, _ := r.events.result()
	if _, fresh := agent.called("session/new"); fresh || output != "new" || session != "ses_prior" || r.retired != "" {
		t.Fatalf("resume: output %q session %q", output, session)
	}
}

func TestACPRefusedLoadRunsFreshOnce(t *testing.T) {
	agent := &fakeAgent{version: "2.2.2", loadFails: true, prompted: make(chan string, 1), updates: []map[string]any{{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "fresh"}}}, stop: "end_turn"}
	task := relayTask(t)
	task.PriorSessionID = "ses_prior"
	r := &running{task: task, resume: "ses_prior", prompt: []byte("You're resuming the prior session")}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	_, session, _ := r.events.result()
	prompt := <-agent.prompted
	if session != "ses_fresh" || r.retired != "ses_prior" || r.resume != "" || !strings.Contains(prompt, "## Session Continuity Notice") || strings.Contains(prompt, "You're resuming") {
		t.Fatalf("fresh run: session %q retired %q prompt %q", session, r.retired, prompt)
	}
}

func TestACPSessionNamesMCPRelayOnly(t *testing.T) {
	agent := &fakeAgent{version: "2.2.2", stop: "end_turn", usage: map[string]any{"inputTokens": 1, "outputTokens": 1, "totalTokens": 2}}
	r := &running{adapter: &Adapter{MCPRelayURL: "http://mcp-relay:8095"}, mcpToken: MCPRelayPrefix + strings.Repeat("a", 64), connections: map[string]Remote{"docs": {Type: "remote", URL: "https://docs.example.invalid/mcp"}}}
	if err := turnWith(t, context.Background(), agent, r); err != nil {
		t.Fatal(err)
	}
	params, _ := agent.called("session/new")
	if !strings.Contains(string(params), `"url":"http://mcp-relay:8095/docs"`) || !strings.Contains(string(params), "Bearer "+r.mcpToken) || strings.Contains(string(params), "docs.example.invalid") {
		t.Fatalf("session/new MCP: %s", params)
	}
	agent.mu.Lock()
	settled := agent.at["session/prompt"].Sub(agent.at["session/new"])
	agent.mu.Unlock()
	if settled < mcpSettle {
		t.Fatalf("prompt %s after session/new; 2.x needs %s to register MCP tools", settled, mcpSettle)
	}
}

func TestACPPermissionRequestsAreRejected(t *testing.T) {
	c := &acpClient{}
	response, err := c.RequestPermission(context.Background(), acpPermission("allow_always", "allow_once", "reject_once"))
	if err != nil || response.Outcome.Selected == nil || response.Outcome.Selected.OptionId != "reject_once" {
		t.Fatalf("permission outcome: %+v %v", response.Outcome, err)
	}
	response, _ = c.RequestPermission(context.Background(), acpPermission("allow_once"))
	if response.Outcome.Cancelled == nil {
		t.Fatal("permission without a reject option was not cancelled")
	}
}

func acpPermission(kinds ...string) acp.RequestPermissionRequest {
	request := acp.RequestPermissionRequest{SessionId: "ses_fresh", ToolCall: acp.ToolCallUpdate{ToolCallId: "call-1"}}
	for _, kind := range kinds {
		request.Options = append(request.Options, acp.PermissionOption{OptionId: acp.PermissionOptionId(kind), Kind: acp.PermissionOptionKind(kind), Name: kind})
	}
	return request
}

func TestToolOutputPreviewIsUTF8Safe(t *testing.T) {
	long := strings.Repeat("я", toolOutputPreview)
	output, truncated := preview(long)
	if !truncated || len(output) > toolOutputPreview || !strings.HasPrefix(long, output) {
		t.Fatalf("preview: %d %v", len(output), truncated)
	}
}

func TestRunningToolPausesIdleWatchdog(t *testing.T) {
	var stream eventStream
	stream.active.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	stream.running.Add(1)
	if stream.idle(time.Minute) {
		t.Fatal("watchdog fired while a tool runs")
	}
	stream.running.Add(-1)
	if !stream.idle(time.Minute) {
		t.Fatal("watchdog paused without a running tool")
	}
}
