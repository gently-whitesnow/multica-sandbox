package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

// ACP transport bounds (ADR 0023).
const (
	acpSetup      = 2 * time.Minute
	cancelGrace   = 10 * time.Second
	exportTimeout = 30 * time.Second
)

// mcpSettle covers 2.x registering session MCP tools after session/new answers: a
// prompt sent at once runs its first step without them (observed below 100 ms on 2.2.2).
var mcpSettle = 2 * time.Second

// streamCut keeps upstream wording so Multica classifies it as a retryable provider_network failure.
var streamCut = &execution.AgentFailure{Message: "opencode stream ended without a terminal signal (no prompt response)"}

// quiet keeps SDK diagnostics, which quote unparsable agent output, out of controller logs.
var quiet = slog.New(slog.DiscardHandler)

// converse drives one `opencode acp` process through one prompt turn. The client
// offers no filesystem or terminal and rejects permission requests; the projected
// configuration remains the permission authority.
func (r *running) converse(ctx context.Context, stdin io.Writer, stdout io.Reader) error {
	client := &acpClient{events: r.events, calls: map[string]*toolCall{}}
	conn := acp.NewClientSideConnection(client, stdin, stdout)
	conn.SetLogger(quiet)
	stop := r.events.flushEvery(reportInterval)
	defer stop()
	setup, cancel := context.WithTimeout(ctx, acpSetup)
	defer cancel()
	init, err := conn.Initialize(setup, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, ClientCapabilities: acp.ClientCapabilities{}})
	if err != nil {
		return r.failure(ctx, conn, err)
	}
	if len(r.connections) != 0 && !init.AgentCapabilities.McpCapabilities.Http {
		return &execution.AgentFailure{Message: "opencode does not accept HTTP MCP servers"}
	}
	r.v2 = init.AgentInfo != nil && major(init.AgentInfo.Version) >= 2
	session, err := r.open(setup, conn, client, init.AgentCapabilities.LoadSession)
	if err != nil {
		return r.failure(ctx, conn, err)
	}
	return r.ask(ctx, conn, client, session)
}

// open continues the prior session, ignoring its replay; any load error retires it
// and the turn runs fresh once with the continuity notice.
func (r *running) open(ctx context.Context, conn *acp.ClientSideConnection, client *acpClient, load bool) (acp.SessionId, error) {
	servers := r.sessionMCP()
	if r.resume != "" {
		var err error = errNoLoad
		if load {
			client.replay(true)
			_, err = conn.LoadSession(ctx, acp.LoadSessionRequest{SessionId: acp.SessionId(r.resume), Cwd: repo.WorkDir, McpServers: servers})
			client.replay(false)
		}
		if err == nil {
			return r.opened(client, acp.SessionId(r.resume))
		}
		if ctx.Err() != nil {
			return "", err
		}
		select {
		case <-conn.Done():
			return "", err
		default:
		}
		prompt, _, perr := Prompt(r.task, r.adapter.Relay != nil, r.adapter.Checkout != nil, Lost)
		if perr != nil {
			return "", perr
		}
		r.prompt, r.retired, r.resume = prompt, r.resume, ""
	}
	created, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: repo.WorkDir, McpServers: servers})
	if err != nil {
		return "", err
	}
	if r.v2 && len(r.connections) != 0 {
		select {
		case <-time.After(mcpSettle):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return r.opened(client, created.SessionId)
}

var errNoLoad = errors.New("agent cannot load sessions")

func (r *running) opened(client *acpClient, id acp.SessionId) (acp.SessionId, error) {
	if !sessionID.MatchString(string(id)) {
		return "", &execution.AgentFailure{Message: "opencode returned an invalid session ID"}
	}
	client.follow(id)
	r.events.begin(string(id))
	r.events.add(multica.Message{Type: "status", Content: "running", CreatedAt: time.Now().UTC()})
	return id, nil
}

type answer struct {
	response acp.PromptResponse
	err      error
}

// ask sends the turn and decides its outcome by stopReason; cancellation sends
// session/cancel before the exec is torn down.
func (r *running) ask(ctx context.Context, conn *acp.ClientSideConnection, client *acpClient, id acp.SessionId) error {
	turn, end := context.WithCancel(context.Background())
	defer end()
	replied := make(chan answer, 1)
	since := time.Now()
	go func() {
		response, err := conn.Prompt(turn, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(string(r.prompt))}})
		replied <- answer{response, err}
	}()
	var a answer
	select {
	case a = <-replied:
	case <-ctx.Done():
		grace, cancel := context.WithTimeout(context.Background(), cancelGrace)
		defer cancel()
		_ = conn.Cancel(grace, acp.CancelNotification{SessionId: id})
		select {
		case <-replied:
		case <-grace.Done():
		}
		return ctx.Err()
	}
	if a.err != nil {
		return r.failure(ctx, conn, a.err)
	}
	usage := r.turnUsage(ctx, id, since, a.response.Usage)
	r.events.reportUsage(usage)
	switch a.response.StopReason {
	case acp.StopReasonEndTurn:
		if !client.produced() && usage == (multica.Usage{}) {
			return &execution.AgentFailure{Message: "opencode turn ended with no text, no tool call and no reported usage — the provider produced nothing"}
		}
		return nil
	case acp.StopReasonCancelled:
		return &execution.AgentFailure{Message: "opencode cancelled the turn"}
	case acp.StopReasonMaxTokens:
		return &execution.AgentFailure{Message: "opencode stopped: the model hit its output length limit"}
	case acp.StopReasonRefusal:
		return &execution.AgentFailure{Message: "opencode stopped: the provider refused the request"}
	}
	return &execution.AgentFailure{Message: "opencode stopped: " + safeWord(string(a.response.StopReason))}
}

// failure withholds native error text, which quotes provider bodies; the error name
// in its data is safe, and the inference relay knows the gateway's refusal status.
func (r *running) failure(ctx context.Context, conn *acp.ClientSideConnection, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var failure *execution.AgentFailure
	if errors.As(err, &failure) {
		return err
	}
	select {
	case <-conn.Done():
		return streamCut
	default:
	}
	var request *acp.RequestError
	if !errors.As(err, &request) {
		return &execution.AgentFailure{Message: "opencode ACP request failed"}
	}
	var data struct {
		ErrorName string `json:"errorName"`
	}
	raw, _ := json.Marshal(request.Data)
	_ = json.Unmarshal(raw, &data)
	// ACP carries no provider status; a refusal as the gateway's last answer explains the failure.
	if r.inferenceToken != "" {
		if status := r.adapter.InferenceRelay.Refusal(r.task.AttemptKey()); status != 0 {
			return &execution.AgentFailure{Status: status}
		}
	}
	switch {
	case data.ErrorName == "provider.rate-limit":
		return &execution.AgentFailure{Status: http.StatusTooManyRequests}
	case request.Code == acpAuthRequired:
		return &execution.AgentFailure{Status: http.StatusUnauthorized}
	case errorName.MatchString(data.ErrorName):
		return &execution.AgentFailure{Message: "opencode reported " + data.ErrorName}
	}
	return &execution.AgentFailure{Message: fmt.Sprintf("opencode ACP request failed (JSON-RPC %d)", request.Code)}
}

// acpAuthRequired is the ACP error 2.x answers when the provider refuses its credential.
const acpAuthRequired = -32000

var errorName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

func safeWord(word string) string {
	if errorName.MatchString(word) {
		return word
	}
	return "unknown reason"
}

func major(version string) int {
	n, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(version, "v"), ".", 2)[0])
	return n
}

// turnUsage takes PromptResponse.usage where the version reports the whole turn
// (2.x), else sums the turn's assistant messages from the session export, which
// 1.x needs: its prompt usage holds only the last model call.
func (r *running) turnUsage(ctx context.Context, id acp.SessionId, since time.Time, reported *acp.Usage) multica.Usage {
	var u multica.Usage
	if reported != nil {
		u.Input, u.Output = int64(max(reported.InputTokens, 0)), int64(max(reported.OutputTokens, 0))
		if reported.ThoughtTokens != nil {
			u.Output += int64(max(*reported.ThoughtTokens, 0))
		}
		if reported.CachedReadTokens != nil {
			u.CacheRead = int64(max(*reported.CachedReadTokens, 0))
		}
		if reported.CachedWriteTokens != nil {
			u.CacheWrite = int64(max(*reported.CachedWriteTokens, 0))
		}
	}
	if r.v2 {
		return u
	}
	exportCtx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	data, err := r.workload.Capture(exportCtx, []string{"opencode", "export", string(id)}, r.environment())
	if exported, ok := exportUsage(data, since.UnixMilli()); err == nil && ok {
		return exported
	}
	return u
}

// exportUsage sums assistant messages created since the turn started; `opencode
// export` may print a banner before its JSON. Reasoning counts as output, as Multica does.
func exportUsage(data []byte, since int64) (multica.Usage, bool) {
	var export struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
				Time struct {
					Created int64 `json:"created"`
				} `json:"time"`
				Tokens *struct {
					Input, Output, Reasoning int64
					Cache                    struct{ Read, Write int64 }
				} `json:"tokens"`
			} `json:"info"`
		} `json:"messages"`
	}
	start := bytes.IndexByte(data, '{')
	if start < 0 || json.Unmarshal(data[start:], &export) != nil {
		return multica.Usage{}, false
	}
	var u multica.Usage
	for _, m := range export.Messages {
		if t := m.Info.Tokens; m.Info.Role == "assistant" && m.Info.Time.Created >= since && t != nil {
			u.Input += max(t.Input, 0)
			u.Output += max(t.Output, 0) + max(t.Reasoning, 0)
			u.CacheRead += max(t.Cache.Read, 0)
			u.CacheWrite += max(t.Cache.Write, 0)
		}
	}
	return u, true
}

// sessionMCP names each selected connection by its relay alias with the attempt's
// opaque relay credential; access tokens stay in the controller.
func (r *running) sessionMCP() []acp.McpServer {
	servers := []acp.McpServer{}
	names := make([]string, 0, len(r.connections))
	for name := range r.connections {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		header := []acp.HttpHeader{{Name: "Authorization", Value: "Bearer " + r.mcpToken}}
		servers = append(servers, acp.McpServer{Http: &acp.McpServerHttpInline{Type: "http", Name: name, Url: r.adapter.MCPRelayURL + "/" + name, Headers: header}})
	}
	return servers
}

type toolCall struct {
	name             string
	emitted, running bool
}

// acpClient maps session updates to Multica messages by toolCallId: only the
// tool_call frame names the tool. Child-session updates keep tool calls and drop text.
type acpClient struct {
	events *eventStream

	mu        sync.Mutex
	session   acp.SessionId
	replaying bool
	text      bool
	calls     map[string]*toolCall
}

func (c *acpClient) replay(on bool) { c.mu.Lock(); c.replaying = on; c.mu.Unlock() }
func (c *acpClient) follow(id acp.SessionId) {
	c.mu.Lock()
	c.session = id
	c.mu.Unlock()
}
func (c *acpClient) produced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text || c.events.tools.Load() > 0
}

// update is the flat shape of the session updates the adapter reads.
type update struct {
	Kind      string          `json:"sessionUpdate"`
	Content   json.RawMessage `json:"content"`
	Call      string          `json:"toolCallId"`
	Status    string          `json:"status"`
	Name      string          `json:"name"`
	Title     string          `json:"title"`
	Tool      string          `json:"kind"`
	RawInput  map[string]any  `json:"rawInput"`
	RawOutput json.RawMessage `json:"rawOutput"`
	Meta      map[string]any  `json:"_meta"`
}

func (c *acpClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.events.touch()
	data, err := json.Marshal(n.Update)
	var u update
	if err != nil || json.Unmarshal(data, &u) != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.replaying || c.session == "" {
		return nil
	}
	_, child := u.Meta["opencode/child-session"]
	child = child || n.SessionId != c.session
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		var block struct {
			Type, Text string
		}
		if child || json.Unmarshal(u.Content, &block) != nil || block.Type != "text" {
			return nil
		}
		kind := "thinking"
		if u.Kind == "agent_message_chunk" {
			kind, c.text = "text", c.text || block.Text != ""
		}
		c.events.chunk(kind, block.Text)
	case "tool_call", "tool_call_update":
		c.tool(u)
	}
	return nil
}

// tool emits tool_use when the tool starts with its input and tool_result when it ends.
func (c *acpClient) tool(u update) {
	call := c.calls[u.Call]
	if call == nil {
		if u.Kind != "tool_call" || u.Call == "" {
			return
		}
		call = &toolCall{name: toolName(u)}
		c.calls[u.Call] = call
	}
	now := time.Now().UTC()
	if !call.emitted && (u.Status == "in_progress" || u.Status == "completed" || u.Status == "failed") {
		call.emitted = true
		c.events.tools.Add(1)
		c.events.add(multica.Message{Type: "tool_use", Tool: call.name, CallID: u.Call, Input: u.RawInput, CreatedAt: now})
	}
	switch u.Status {
	case "in_progress":
		if !call.running {
			call.running = true
			c.events.running.Add(1)
		}
	case "completed", "failed":
		if call.running {
			c.events.running.Add(-1)
		}
		delete(c.calls, u.Call)
		output, truncated := preview(toolOutput(u))
		c.events.add(multica.Message{Type: "tool_result", Tool: call.name, CallID: u.Call, Output: output, OutputTruncated: &truncated, CreatedAt: now})
	}
}

// toolName keeps OpenCode's own tool ids, as the run transport reported them.
func toolName(u update) string {
	if name, ok := u.Meta["opencode/tool"].(string); ok && name != "" {
		return name
	}
	for _, name := range []string{u.Name, u.Title, u.Tool} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return "tool"
}

// toolOutput reads text content blocks, else the raw output or its error.
func toolOutput(u update) string {
	var blocks []struct {
		Type    string `json:"type"`
		Content struct {
			Type, Text string
		} `json:"content"`
	}
	var text strings.Builder
	if json.Unmarshal(u.Content, &blocks) == nil {
		for _, block := range blocks {
			if block.Type == "content" && block.Content.Type == "text" {
				text.WriteString(block.Content.Text)
			}
		}
	}
	if text.Len() > 0 || len(u.RawOutput) == 0 {
		return text.String()
	}
	var raw struct {
		Output *string `json:"output"`
		Error  *string `json:"error"`
	}
	var plain string
	switch {
	case json.Unmarshal(u.RawOutput, &plain) == nil:
		return plain
	case json.Unmarshal(u.RawOutput, &raw) == nil && raw.Output != nil:
		return *raw.Output
	case raw.Error != nil:
		return *raw.Error
	}
	return string(u.RawOutput)
}

// RequestPermission rejects once: the projected permissions already decide every tool.
func (c *acpClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	for _, option := range p.Options {
		if option.Kind == acp.PermissionOptionKindRejectOnce {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{Outcome: "cancelled"}}}, nil
}

var errUnoffered = acp.NewMethodNotFound("client capability not offered")

func (c *acpClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errUnoffered
}
func (c *acpClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errUnoffered
}
func (c *acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errUnoffered
}
func (c *acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errUnoffered
}
func (c *acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errUnoffered
}
func (c *acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errUnoffered
}
func (c *acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errUnoffered
}
