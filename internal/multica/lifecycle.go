package multica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"net/http"
	"time"
)

type Runtime struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// Task excludes arbitrary claim payloads; AuthToken stays redacted and in memory (ADR 0014).
type Task struct {
	AuthToken             Secret            `json:"auth_token"`
	TriggerCommentID      string            `json:"trigger_comment_id"`
	TriggerThreadID       string            `json:"trigger_thread_id"`
	TriggerAuthorType     string            `json:"trigger_author_type"`
	TriggerAuthorName     string            `json:"trigger_author_name"`
	AgentID               string            `json:"agent_id"`
	Agent                 *Agent            `json:"agent"`
	IssueID               string            `json:"issue_id"`
	WorkspaceContext      string            `json:"workspace_context"`
	ChatMessage           string            `json:"chat_message"`
	ProjectTitle          string            `json:"project_title"`
	ProjectDescription    string            `json:"project_description"`
	Repos                 []Repository      `json:"repos"`
	PriorSessionID        string            `json:"prior_session_id"`
	TriggerCommentContent string            `json:"trigger_comment_content"`
	RemoteMCPConnections  []json.RawMessage `json:"remote_mcp_connections"`
	WorkspaceID           string            `json:"workspace_id"`
	ID                    string            `json:"id"`
	RuntimeID             string            `json:"runtime_id"`
	DispatchedAt          string            `json:"dispatched_at"`
	StartClaimSupported   bool              `json:"start_claim_supported"`
}

type Recovery struct {
	Orphaned int `json:"orphaned"`
	Retried  int `json:"retried"`
}

func (c *Client) Register(ctx context.Context, workspace, daemon string) (Runtime, error) {
	if !validID(workspace) || !validID(daemon) {
		return Runtime{}, fmt.Errorf("workspace and daemon must be UUIDs")
	}
	return c.RegisterProvider(ctx, workspace, daemon, ProbeProvider)
}

func (c *Client) RegisterProvider(ctx context.Context, workspace, daemon, provider string) (Runtime, error) {
	if !validID(workspace) || !validID(daemon) || (provider != ProbeProvider && provider != "opencode") {
		return Runtime{}, fmt.Errorf("invalid runtime registration")
	}
	var out struct {
		Runtimes []Runtime `json:"runtimes"`
	}
	err := c.call(ctx, http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": workspace, "daemon_id": daemon, "device_name": "multica-sandbox",
		"runtimes": []map[string]string{{"name": "Sandbox experimental " + provider, "type": provider, "version": "experimental", "status": "online"}},
	}, &out)
	if err != nil {
		return Runtime{}, err
	}
	if len(out.Runtimes) != 1 || !validID(out.Runtimes[0].ID) || out.Runtimes[0].Provider != provider {
		return Runtime{}, fmt.Errorf("unexpected runtime registration")
	}
	return out.Runtimes[0], nil
}

func (c *Client) Recover(ctx context.Context, runtime string) (Recovery, error) {
	var out Recovery
	err := c.runtimePost(ctx, runtime, "/recover-orphans", &out)
	return out, err
}
func (c *Client) Heartbeat(ctx context.Context, runtime string) error {
	return c.call(ctx, http.MethodPost, "/api/daemon/heartbeat", map[string]string{"runtime_id": runtime}, nil)
}
func (c *Client) Claim(ctx context.Context, runtime string) (*Task, error) {
	var out struct {
		Task *Task `json:"task"`
	}
	if err := c.runtimePost(ctx, runtime, "/tasks/claim", &out); err != nil {
		return nil, err
	}
	if out.Task != nil {
		t := out.Task
		if !validID(t.ID) || t.RuntimeID != runtime || !t.StartClaimSupported {
			return nil, fmt.Errorf("unsupported or mismatched claim")
		}
		if _, err := time.Parse(time.RFC3339Nano, t.DispatchedAt); err != nil {
			return nil, fmt.Errorf("claim lacks valid dispatch timestamp")
		}
	}
	return out.Task, nil
}
func (c *Client) RenewPreparation(ctx context.Context, t Task) error {
	if !validID(t.ID) {
		return fmt.Errorf("invalid task ID")
	}
	return c.runtimePost(ctx, t.RuntimeID, "/tasks/"+t.ID+"/prepare-lease", nil)
}

// Start is fenced by dispatched_at, so the upstream daemon replays it on transient failure.
func (c *Client) Start(ctx context.Context, t Task) error {
	return retry(ctx, startRetry, func() error {
		return c.taskPost(ctx, t.ID, "start", map[string]any{"runtime_id": t.RuntimeID, "dispatched_at": t.DispatchedAt, "capabilities": []string{}})
	})
}
func (c *Client) Status(ctx context.Context, id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid task ID")
	}
	var out struct {
		Status string `json:"status"`
	}
	err := c.call(ctx, http.MethodGet, "/api/daemon/tasks/"+id+"/status", nil, &out)
	return out.Status, err
}
func (c *Client) Message(ctx context.Context, id string) error {
	return c.taskPost(ctx, id, "messages", map[string]any{"messages": []map[string]any{{"seq": 1, "type": "text", "content": "Test execution started; this runtime does not implement an agent adapter.", "created_at": time.Now().UTC()}}})
}
func (c *Client) Complete(ctx context.Context, id string, _ execution.Result) error {
	return c.Deliver(ctx, Terminal{Task: id, Output: "Test execution completed; this does not complete the requested agent work."})
}
func (c *Client) Fail(ctx context.Context, id string, cause error, _ execution.Result) error {
	t := Terminal{Task: id, Failed: true, Error: "Lifecycle probe failure"}
	var timeout *execution.TimeoutError
	if errors.As(cause, &timeout) {
		t.Error, t.Reason = "Probe execution "+timeout.Error(), "timeout"
	}
	return c.Deliver(ctx, t)
}
func (c *Client) CancelAck(ctx context.Context, id string) error {
	return retry(ctx, terminalRetry, func() error { return c.taskPost(ctx, id, "cancel-ack", map[string]any{}) })
}
func (c *Client) runtimePost(ctx context.Context, id, suffix string, out any) error {
	if !validID(id) {
		return fmt.Errorf("invalid runtime ID")
	}
	return c.call(ctx, http.MethodPost, "/api/daemon/runtimes/"+id+suffix, map[string]any{}, out)
}
func (c *Client) taskPost(ctx context.Context, id, action string, body any) error {
	if !validID(id) {
		return fmt.Errorf("invalid task ID")
	}
	return c.call(ctx, http.MethodPost, "/api/daemon/tasks/"+id+"/"+action, body, nil)
}

func (c *Client) AgentMessage(ctx context.Context, id string) error {
	return c.taskPost(ctx, id, "messages", map[string]any{"messages": []map[string]any{{"seq": 1, "type": "text", "content": "Experimental OpenCode attempt started.", "created_at": time.Now().UTC()}}})
}
func (c *Client) AgentComplete(ctx context.Context, id string, result execution.Result) error {
	return c.Deliver(ctx, Terminal{Task: id, Output: result.Output, Session: result.SessionID, Disposable: result.Disposable})
}

// AgentFail sends safe text; an empty reason lets Multica classify it as it does for its own daemon.
func (c *Client) AgentFail(ctx context.Context, id, message, reason string, result execution.Result) error {
	return c.Deliver(ctx, Terminal{Task: id, Failed: true, Error: message, Reason: reason, Session: result.SessionID, Disposable: result.Disposable})
}

type Repository struct {
	URL         string `json:"url"`
	Description string `json:"description"`
	Ref         string `json:"ref"`
}
