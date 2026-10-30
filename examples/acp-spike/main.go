// Command acp-spike drives `opencode acp` in a disposable container with the ACP Go SDK
// and prints what the transport offers the OpenCode adapter. It is a spike, not a test:
// see README.md for the questions it answers.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

const (
	repository = "ghcr.io/anomalyco/opencode"
	user       = "65532:65532"
	workDir    = "/workspace/work"
)

// candidates pin official releases the spike exercises before conformance; images.txt
// lists only verified releases, which the controller accepts (ADR 0023).
var candidates = map[string]string{
	"2.2.2": "sha256:7c8cd91b6055e39306fa9040b9abf4e7d2808b1fccfe2d0389e3f0cdc654a048",
}

func main() {
	version := flag.String("version", "", "candidate release from candidates; default: newest verified image")
	image := flag.String("image", "", "OpenCode image; overrides -version")
	host := flag.String("host", "host.docker.internal", "host name the container uses to reach the fixtures")
	keep := flag.Bool("keep", false, "keep the container for inspection")
	flag.BoolVar(&debug, "debug", false, "log fixture requests")
	flag.BoolVar(&acpMCP, "acp-mcp", false, "also pass the MCP fixture through session/new with a static header")
	flag.Parse()
	if *image == "" {
		*image = opencode.Images[len(opencode.Images)-1]
		if *version != "" {
			digest, ok := candidates[*version]
			if !ok {
				fmt.Fprintln(os.Stderr, "acp-spike: unknown candidate", *version)
				os.Exit(2)
			}
			*image = repository + ":" + *version + "@" + digest
		}
	}
	if err := run(*image, *host, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "acp-spike:", err)
		os.Exit(1)
	}
}

type spike struct {
	container string
	// v2 drops `opencode acp --cwd`; the exec working directory and session cwd remain.
	v2      bool
	mcpURL  string
	seen    *bearers
	results []string
}

func (s *spike) check(name string, ok bool, detail string, args ...any) {
	verdict := "PASS"
	if !ok {
		verdict = "FAIL"
	}
	line := fmt.Sprintf("%s %-28s %s", verdict, name, fmt.Sprintf(detail, args...))
	s.results = append(s.results, line)
	fmt.Println(line)
}

var acpMCP bool

// sessionMCP is the ACP channel for MCP: headers are fixed for the session's lifetime.
func (s *spike) sessionMCP() []acp.McpServer {
	if !acpMCP {
		return []acp.McpServer{}
	}
	return []acp.McpServer{{Http: &acp.McpServerHttpInline{Type: "http", Name: "viaacp", Url: s.mcpURL, Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer acp-header"}}}}}
}

func run(image, host string, keep bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s := &spike{container: fmt.Sprintf("acp-spike-%d", time.Now().UnixNano()), seen: &bearers{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", inference)
	mux.Handle("/mcp", mcpFixture(s.seen))
	go http.Serve(listener, mux)
	origin := fmt.Sprintf("http://%s:%d", host, listener.Addr().(*net.TCPAddr).Port)
	s.mcpURL = origin + "/mcp"

	if err := s.launch(ctx, image); err != nil {
		return err
	}
	if !keep {
		defer docker(context.Background(), nil, "rm", "-f", s.container)
	}
	if err := s.project(ctx, origin); err != nil {
		return err
	}
	a, id, err := s.handshake(ctx)
	if err != nil {
		return err
	}
	if err := s.toolTurn(ctx, a, id); err != nil {
		return err
	}
	if err := s.rotate(ctx, a, id); err != nil {
		return err
	}
	a.stop()
	s.exportUsage(ctx, id)
	b, err := s.resume(ctx, id)
	if err != nil {
		return err
	}
	s.deniedTurn(ctx, b, id)
	s.cancelTurn(ctx, b, id)
	b.stop()
	listening, _ := s.exec(ctx, nil, "/bin/sh", "-c", "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk '$4==\"0A\"' | wc -l")
	fmt.Printf("listening sockets after stop: %s\n", strings.TrimSpace(string(listening)))

	fmt.Println("\nsummary")
	for _, line := range s.results {
		fmt.Println(line)
	}
	return nil
}

// launch starts the attempt shape that matters here: non-root, read-only rootfs, tmpfs workspace, no capabilities.
func (s *spike) launch(ctx context.Context, image string) error {
	if _, err := docker(ctx, nil, "run", "-d", "--name", s.container, "--user", user, "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--tmpfs", "/workspace:rw,exec,mode=1777", "--tmpfs", "/tmp:rw,mode=1777",
		"--add-host", "host.docker.internal:host-gateway",
		"--env", "XDG_DATA_HOME=/workspace/data", "--env", "XDG_CONFIG_HOME=/workspace/config", "--env", "XDG_CACHE_HOME=/workspace/cache",
		"--env", "XDG_STATE_HOME=/workspace/state", "--env", "HOME=/workspace/home", "--env", "OPENCODE_DISABLE_AUTOUPDATE=true",
		"--env", "OPENCODE_DISABLE_MODELS_FETCH=true", "--env", "OPENCODE_DISABLE_DEFAULT_PLUGINS=true", "--env", "OPENCODE_DISABLE_LSP_DOWNLOAD=true",
		"--entrypoint", "/bin/sh", image, "-c", "exec sleep 3600"); err != nil {
		return err
	}
	version, _ := s.exec(ctx, nil, "opencode", "--version")
	s.v2 = strings.Contains(string(version), "v2.")
	fmt.Printf("image %s\nopencode %s\n", image, strings.TrimSpace(string(version)))
	return nil
}

// project writes the adapter's projection: deny-by-default permissions, a remote MCP from
// opencode.json with its access token in the native auth store, and a fixed provider key.
func (s *spike) project(ctx context.Context, origin string) error {
	config, err := opencode.Config(map[string]opencode.Remote{"spike": {Type: "remote", URL: s.mcpURL}}, true)
	if err != nil {
		return err
	}
	var projected map[string]any
	_ = json.Unmarshal(config, &projected)
	projected["model"] = "fixture/model"
	projected["enabled_providers"] = []string{"fixture"}
	projected["provider"] = map[string]any{"fixture": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Fixture",
		"options": map[string]string{"baseURL": origin + "/v1", "apiKey": "inf_relay_fixture"}, "models": map[string]any{"model": map[string]any{"name": "model"}}}}
	config, _ = json.Marshal(projected)
	if _, err := s.exec(ctx, nil, "/bin/sh", "-c", "mkdir -p /workspace/work /workspace/home /workspace/data/opencode /workspace/config/opencode"); err != nil {
		return err
	}
	if err := s.write(ctx, "/workspace/opencode.json", config); err != nil {
		return err
	}
	return s.writeToken(ctx, "token-one")
}

func (s *spike) handshake(ctx context.Context) (*agent, acp.SessionId, error) {
	a, err := s.start(ctx)
	if err != nil {
		return nil, "", err
	}
	init, err := a.conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{Fs: acp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false}, Terminal: false}})
	if err != nil {
		return nil, "", fmt.Errorf("initialize: %w (stderr: %s)", err, a.stderr())
	}
	caps, _ := json.Marshal(init.AgentCapabilities)
	fmt.Printf("agentCapabilities %s\n", caps)
	s.check("loadSession", init.AgentCapabilities.LoadSession, "advertised=%v", init.AgentCapabilities.LoadSession)
	session, err := a.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: workDir, McpServers: s.sessionMCP()})
	if err != nil {
		return nil, "", fmt.Errorf("session/new: %w (stderr: %s)", err, a.stderr())
	}
	id := session.SessionId
	s.check("session id shape", strings.HasPrefix(string(id), "ses_"), "%s (adapter regexp %s)", id, `^ses_[A-Za-z0-9]{1,64}$`)
	return a, id, nil
}

// toolTurn checks that a silent tool is visible while it runs (GH #9614) and MCP uses the auth store.
func (s *spike) toolTurn(ctx context.Context, a *agent, id acp.SessionId) error {
	start := time.Now()
	reply, err := a.conn.Prompt(ctx, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(markTool + " probe then build")}})
	if err != nil {
		return fmt.Errorf("prompt tool: %w (stderr: %s)", err, a.stderr())
	}
	elapsed := time.Since(start)
	events := a.client.drain()
	s.check("stopReason end_turn", reply.StopReason == acp.StopReasonEndTurn, "stopReason=%s after %s", reply.StopReason, elapsed.Round(time.Millisecond))
	shell := "bash"
	if s.v2 {
		shell = "shell"
	}
	running, done := toolTimes(events, shell)
	s.check("tool visible in flight", !running.IsZero() && !done.IsZero() && done.Sub(running) >= (buildSeconds-1)*time.Second,
		"in_progress→completed %s (tool sleeps %ds)", done.Sub(running).Round(time.Millisecond), buildSeconds)
	s.check("tool input on in_progress", inputBeforeDone(events, shell), "rawInput carries the command before completion")
	s.check("MCP via auth store", contains(s.seen.list(), "token-one"), "MCP bearers seen: %q", s.seen.list())
	s.check("text streamed", strings.Contains(text(events), "tool turn done"), "agent text %q", text(events))
	// The fixture reports 18 tokens per model call and this turn makes three calls.
	usage, _ := json.Marshal(reply.Usage)
	s.check("prompt usage covers turn", reply.Usage != nil && reply.Usage.TotalTokens >= 3*18, "PromptResponse.usage=%s; usage updates %d", usage, len(kinds(events, "usage_update")))
	fmt.Printf("update kinds: %s\n", strings.Join(kindSequence(events), " "))
	s.check("no permission prompts", a.client.permissions() == 0, "session/request_permission calls: %d", a.client.permissions())
	return nil
}

// rotate rewrites the auth store while a tool of the same prompt runs, as the controller
// does during one long attempt.
func (s *spike) rotate(ctx context.Context, a *agent, id acp.SessionId) error {
	before := len(s.seen.list())
	rotated := make(chan error, 1)
	go func() {
		r, err := a.conn.Prompt(ctx, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(markRotate)}})
		second, _ := json.Marshal(r.Usage)
		fmt.Printf("second turn PromptResponse.usage=%s (turn makes 3 calls, 54 tokens)\n", second)
		rotated <- err
	}()
	if !a.client.waitStatus(ctx, "in_progress", 30*time.Second) {
		return fmt.Errorf("rotate: tool never started")
	}
	if err := s.writeToken(ctx, "token-two"); err != nil {
		return err
	}
	if err := <-rotated; err != nil {
		return fmt.Errorf("prompt rotate: %w", err)
	}
	a.client.drain()
	after := s.seen.list()[before:]
	s.check("MCP rotation mid-turn", len(after) > 0 && !contains(after, "token-one") && contains(after, "token-two"), "MCP bearers after rewrite: %q", after)
	return nil
}

// exportUsage reads usage from the session export after the process exits, like upstream.
func (s *spike) exportUsage(ctx context.Context, id acp.SessionId) {
	command := `cd /workspace/work && exec opencode export "$0"`
	if s.v2 {
		command = `cd /workspace/work && exec opencode session export "$0" --standalone`
	}
	export, err := s.exec(ctx, nil, "/bin/sh", "-c", command, string(id))
	tokens := exportTokens(export)
	s.check("export usage", err == nil && tokens > 0, "assistant tokens in export: %d (err=%v)", tokens, err)
}

// resume loads the session in a new process, as a new attempt on the retained workdir would.
func (s *spike) resume(ctx context.Context, id acp.SessionId) (*agent, error) {
	b, err := s.start(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = b.conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		return nil, err
	}
	_, err = b.conn.LoadSession(ctx, acp.LoadSessionRequest{SessionId: id, Cwd: workDir, McpServers: s.sessionMCP()})
	replayed := b.client.drain()
	s.check("session/load", err == nil, "err=%v, replayed updates: %d", err, len(replayed))
	if err == nil {
		if _, err = b.conn.Prompt(ctx, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(markResume)}}); err != nil {
			return nil, fmt.Errorf("prompt resume: %w", err)
		}
		got := text(b.client.drain())
		s.check("resumed context", strings.Contains(got, "prompts seen: 3"), "agent text %q", got)
	}
	_, err = b.conn.LoadSession(ctx, acp.LoadSessionRequest{SessionId: "ses_doesnotexist0000", Cwd: workDir, McpServers: []acp.McpServer{}})
	s.check("unknown session rejected", err != nil, "err=%v", err)
	return b, nil
}

// deniedTurn calls a tool outside the projected allow-list; it must not run.
func (s *spike) deniedTurn(ctx context.Context, b *agent, id acp.SessionId) {
	b.client.drain()
	_, err := b.conn.Prompt(ctx, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(markDenied)}})
	events := b.client.drain()
	call := firstCall(events, deniedTool)
	status := ""
	for _, e := range events {
		if call != "" && e.call == call && e.status != "" {
			status = e.status
		}
	}
	fmt.Printf("denied turn: %s\n", strings.Join(kindSequence(events), " "))
	s.check("denied tool does not run", status != "completed", "call=%q final status=%q permission requests=%d err=%v", call, status, b.client.permissions(), err)
}

// cancelTurn checks that session/cancel stops the running tool and answers the prompt.
func (s *spike) cancelTurn(ctx context.Context, b *agent, id acp.SessionId) {
	hang := make(chan acp.PromptResponse, 1)
	go func() {
		r, _ := b.conn.Prompt(ctx, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(markCancel)}})
		hang <- r
	}()
	if !b.client.waitStatus(ctx, "in_progress", 30*time.Second) {
		s.check("cancel", false, "tool never reached in_progress")
		return
	}
	sent := time.Now()
	_ = b.conn.Cancel(ctx, acp.CancelNotification{SessionId: id})
	select {
	case r := <-hang:
		s.check("cancel answers prompt", r.StopReason == acp.StopReasonCancelled, "stopReason=%s after %s", r.StopReason, time.Since(sent).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		s.check("cancel answers prompt", false, "no answer 15s after session/cancel")
	}
	time.Sleep(time.Second)
	ps, _ := s.exec(ctx, nil, "/bin/sh", "-c", "ps -eo args 2>/dev/null || ps")
	s.check("cancel stops the tool", !strings.Contains(string(ps), "sleep 120"), "sleep 120 still running=%v", strings.Contains(string(ps), "sleep 120"))
}

// agent is one `opencode acp` process reached through `docker exec -i`.
type agent struct {
	cmd    *exec.Cmd
	stdin  io.Closer
	conn   *acp.ClientSideConnection
	client *client
	errBuf *bytes.Buffer
}

func (s *spike) start(ctx context.Context) (*agent, error) {
	args := []string{"exec", "-i", "--user", user, "--workdir", workDir, s.container, "opencode", "acp"}
	if !s.v2 {
		args = append(args, "--cwd", workDir)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	a := &agent{cmd: cmd, stdin: stdin, client: &client{}, errBuf: &bytes.Buffer{}}
	cmd.Stderr = a.errBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a.conn = acp.NewClientSideConnection(a.client, stdin, stdout)
	return a, nil
}

func (a *agent) stderr() string {
	text := a.errBuf.String()
	if len(text) > 2000 {
		text = text[len(text)-2000:]
	}
	return text
}

// stop closes stdin and waits; the exec client exits when the agent does.
func (a *agent) stop() {
	_ = a.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = a.cmd.Process.Kill()
		<-done
	}
}

type event struct {
	at     time.Time
	kind   string
	tool   string
	call   string
	status string
	input  bool
	text   string
}

// client is the controller side: no filesystem or terminal, and no interactive approval.
type client struct {
	mu      sync.Mutex
	events  []event
	asked   int
	changed chan struct{}
}

func (c *client) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	data, _ := json.Marshal(n.Update)
	var u struct {
		Kind     string `json:"sessionUpdate"`
		Call     string `json:"toolCallId"`
		Status   string `json:"status"`
		Title    string `json:"title"`
		RawInput any    `json:"rawInput"`
		Content  struct {
			Text string `json:"text"`
		} `json:"content"`
		Meta map[string]any `json:"_meta"`
	}
	_ = json.Unmarshal(data, &u)
	tool := u.Title
	if name, ok := u.Meta["opencode/tool"].(string); ok {
		tool = name
	}
	input := false
	if m, ok := u.RawInput.(map[string]any); ok && len(m) > 0 {
		input = true
	}
	c.mu.Lock()
	c.events = append(c.events, event{at: time.Now(), kind: u.Kind, tool: tool, call: u.Call, status: u.Status, input: input, text: u.Content.Text})
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
	c.mu.Unlock()
	return nil
}

func (c *client) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.asked++
	c.mu.Unlock()
	for _, option := range p.Options {
		if option.Kind == acp.PermissionOptionKindRejectOnce {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{Outcome: "cancelled"}}}, nil
}

var errUnsupported = errors.New("not offered by the controller")

func (c *client) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errUnsupported
}
func (c *client) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errUnsupported
}
func (c *client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errUnsupported
}
func (c *client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errUnsupported
}
func (c *client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errUnsupported
}
func (c *client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errUnsupported
}
func (c *client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errUnsupported
}

// drain returns the updates so far after a short settle for trailing notifications.
func (c *client) drain() []event {
	time.Sleep(300 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	events := c.events
	c.events = nil
	return events
}

func (c *client) permissions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

func (c *client) waitStatus(ctx context.Context, status string, limit time.Duration) bool {
	deadline := time.After(limit)
	for {
		c.mu.Lock()
		for _, e := range c.events {
			if e.status == status {
				c.mu.Unlock()
				return true
			}
		}
		changed := make(chan struct{})
		c.changed = changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// firstCall finds the first call of a tool; only its start frame carries the tool name
// (1.x titles later updates with the command), so updates are matched by call ID.
func firstCall(events []event, tool string) string {
	for _, e := range events {
		if e.kind == "tool_call" && e.tool == tool {
			return e.call
		}
	}
	return ""
}

// toolTimes finds when the first call of a tool started running and when it completed.
func toolTimes(events []event, tool string) (running, done time.Time) {
	call := firstCall(events, tool)
	for _, e := range events {
		if call == "" || e.call != call {
			continue
		}
		if e.status == "in_progress" && running.IsZero() {
			running = e.at
		}
		if e.status == "completed" || e.status == "failed" {
			done = e.at
		}
	}
	return running, done
}

func inputBeforeDone(events []event, tool string) bool {
	call := firstCall(events, tool)
	for _, e := range events {
		if call != "" && e.call == call && e.status == "in_progress" && e.input {
			return true
		}
	}
	return false
}

func text(events []event) string {
	var b strings.Builder
	for _, e := range events {
		if e.kind == "agent_message_chunk" {
			b.WriteString(e.text)
		}
	}
	return b.String()
}

func kinds(events []event, kind string) []string {
	var out []string
	for _, e := range events {
		if e.kind == kind {
			out = append(out, e.kind)
		}
	}
	return out
}

func kindSequence(events []event) []string {
	var out []string
	for _, e := range events {
		k := e.kind
		if e.status != "" {
			k += "(" + e.tool + ":" + e.status + ")"
		}
		if len(out) == 0 || out[len(out)-1] != k {
			out = append(out, k)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// exportTokens sums assistant tokens in `opencode export`, which may print a banner before the JSON.
func exportTokens(data []byte) int64 {
	start := bytes.IndexByte(data, '{')
	if start < 0 {
		return 0
	}
	var export struct {
		Messages []struct {
			Info struct {
				Role   string `json:"role"`
				Tokens struct {
					Input  int64 `json:"input"`
					Output int64 `json:"output"`
				} `json:"tokens"`
			} `json:"info"`
		} `json:"messages"`
	}
	if json.Unmarshal(data[start:], &export) != nil {
		return 0
	}
	var total int64
	for _, m := range export.Messages {
		if m.Info.Role == "assistant" {
			total += m.Info.Tokens.Input + m.Info.Tokens.Output
		}
	}
	return total
}

func (s *spike) exec(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return docker(ctx, stdin, append([]string{"exec", "-i", "--user", user, s.container}, args...)...)
}

func (s *spike) write(ctx context.Context, path string, data []byte) error {
	_, err := s.exec(ctx, data, "/bin/sh", "-c", `umask 077; cat > "$0.next" && mv -f "$0.next" "$0"`, path)
	return err
}

func (s *spike) writeToken(ctx context.Context, token string) error {
	store, _ := json.Marshal(map[string]opencode.Entry{"spike": {ServerURL: s.mcpURL, Tokens: opencode.Token{AccessToken: token, ExpiresAt: time.Now().Add(time.Hour).Unix()}}})
	return s.write(ctx, opencode.AuthPath, store)
}

func docker(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
