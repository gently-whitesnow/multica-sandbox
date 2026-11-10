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
	AuthToken              Secret            `json:"auth_token"`
	TriggerCommentID       string            `json:"trigger_comment_id"`
	TriggerThreadID        string            `json:"trigger_thread_id"`
	TriggerAuthorType      string            `json:"trigger_author_type"`
	TriggerAuthorName      string            `json:"trigger_author_name"`
	AgentID                string            `json:"agent_id"`
	Agent                  *Agent            `json:"agent"`
	IssueID                string            `json:"issue_id"`
	WorkspaceContext       string            `json:"workspace_context"`
	ChatMessage            string            `json:"chat_message"`
	ProjectID              string            `json:"project_id"`
	ProjectTitle           string            `json:"project_title"`
	ProjectDescription     string            `json:"project_description"`
	ProjectResources       []ProjectResource `json:"project_resources"`
	Repos                  []Repository      `json:"repos"`
	PriorSessionID         string            `json:"prior_session_id"`
	PriorWorkDir           string            `json:"prior_work_dir"`
	TriggerCommentEditedAt string            `json:"trigger_comment_edited_at"`
	TriggerCommentRevision int64             `json:"trigger_comment_revision"`
	TriggerCommentContent  string            `json:"trigger_comment_content"`
	RemoteMCPConnections   []json.RawMessage `json:"remote_mcp_connections"`
	WorkspaceID            string            `json:"workspace_id"`
	ID                     string            `json:"id"`
	RuntimeID              string            `json:"runtime_id"`
	DispatchedAt           string            `json:"dispatched_at"`
	StartClaimSupported    bool              `json:"start_claim_supported"`

	// Resume deltas and coalesced comments feed the upstream per-turn hints.
	CoalescedComments             []Comment `json:"coalesced_comments"`
	CoalescedCommentIDs           []string  `json:"coalesced_comment_ids"`
	IssueChangedFields            []string  `json:"issue_changed_fields"`
	NewCommentsSince              string    `json:"new_comments_since"`
	IssueStatus                   string    `json:"issue_status"`
	IssueAssigneeType             string    `json:"issue_assignee_type"`
	IssueAssigneeID               string    `json:"issue_assignee_id"`
	NewCommentCount               int       `json:"new_comment_count"`
	NewCommentsDeltaKnown         bool      `json:"new_comments_delta_known"`
	IssueStateDeltaKnown          bool      `json:"issue_state_delta_known"`
	PriorSessionResumeUnavailable bool      `json:"prior_session_resume_unavailable"`

	// Native run-only autopilot claims have no issue.
	AutopilotRunID          string           `json:"autopilot_run_id"`
	AutopilotID             string           `json:"autopilot_id"`
	AutopilotTitle          string           `json:"autopilot_title"`
	AutopilotDescription    string           `json:"autopilot_description"`
	AutopilotSource         string           `json:"autopilot_source"`
	AutopilotTriggerPayload json.RawMessage  `json:"autopilot_trigger_payload"`
	WakeupID                string           `json:"wakeup_id"`
	WakeupSystemRule        string           `json:"wakeup_system_rule"`
	WakeupJoined            string           `json:"wakeup_joined"`
	HandoffNote             string           `json:"handoff_note"`
	InitiatorName           string           `json:"initiator_name"`
	InitiatorEmail          string           `json:"initiator_email"`
	ProjectWorkflow         *ProjectWorkflow `json:"project_workflow"`

	// Chat claims carry the upstream chat prompt inputs (ADR 0017).
	ChatSessionID            string       `json:"chat_session_id"`
	ChatChannelType          string       `json:"chat_channel_type"`
	ChatInThread             bool         `json:"chat_in_thread"`
	ChatChannelDeliversFiles bool         `json:"chat_channel_delivers_files"`
	ChatType                 string       `json:"chat_type"`
	ChatMessageAttachments   []Attachment `json:"chat_message_attachments"`
	ChatIntro                bool         `json:"chat_intro"`
}

type ProjectWorkflow struct {
	Brief string `json:"brief"`
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
func (c *Client) Claim(ctx context.Context, runtime string) (*Task, error) {
	var out struct {
		Task *Task `json:"task"`
	}
	if !validID(runtime) {
		return nil, fmt.Errorf("invalid runtime ID")
	}
	if err := c.callLimit(ctx, http.MethodPost, "/api/daemon/runtimes/"+runtime+"/tasks/claim", map[string]any{}, &out, claimLimit); err != nil {
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
	if !validID(t.ID) {
		return fmt.Errorf("invalid task ID")
	}
	// Offered only where the server can also resolve lost responses (ADR 0036).
	offer := c.supplements != nil && t.ValidAttempt() && validID(t.IssueID) && c.supplementReceipts(ctx, t)
	negotiated, err := c.startTask(ctx, t, offer)
	if c.supplements != nil {
		c.supplements.negotiate(t, err == nil && negotiated)
	}
	return err
}

func (c *Client) startTask(ctx context.Context, t Task, offer bool) (bool, error) {
	caps := []string{}
	if offer {
		caps = append(caps, taskSupplementCapability)
	}
	negotiated := false
	err := retry(ctx, startRetry, func(ctx context.Context) error {
		read := 0
		if offer {
			read = responseLimit
		}
		data, err := c.send(ctx, http.MethodPost, "/api/daemon/tasks/"+t.ID+"/start", map[string]any{"runtime_id": t.RuntimeID, "dispatched_at": t.DispatchedAt, "capabilities": caps}, read)
		if err != nil || !offer {
			return err
		}
		var response struct {
			Capability string `json:"supplement_capability"`
		}
		if len(data) != 0 && json.Unmarshal(data, &response) != nil {
			return fmt.Errorf("invalid Multica start response")
		}
		negotiated = offer && response.Capability == taskSupplementCapability
		return nil
	})
	return negotiated, err
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
	err := retry(ctx, terminalRetry, func(ctx context.Context) error { return c.taskPost(ctx, id, "cancel-ack", map[string]any{}) })
	if err == nil && c.supplements != nil {
		c.supplements.retire(id)
	}
	return err
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

func (c *Client) AgentComplete(ctx context.Context, id string, result execution.Result) error {
	return c.Deliver(ctx, Terminal{Task: id, Output: result.Output, Session: result.SessionID, WorkDir: result.WorkDir, Retired: result.RetiredSessionID, Disposable: result.Disposable})
}

// AgentFail sends safe text; an empty reason lets Multica classify it as it does for its own daemon.
func (c *Client) AgentFail(ctx context.Context, id, message, reason string, result execution.Result) error {
	return c.Deliver(ctx, Terminal{Task: id, Failed: true, Error: message, Reason: reason, Session: result.SessionID, WorkDir: result.WorkDir, Retired: result.RetiredSessionID, Disposable: result.Disposable})
}

type Repository struct {
	URL         string `json:"url"`
	Description string `json:"description"`
	Ref         string `json:"ref"`
}

// Attachment is a chat message attachment the agent downloads by ID.
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

// Comment is an earlier comment the server folded into a comment-triggered run.
type Comment struct {
	ID         string `json:"id"`
	ThreadID   string `json:"thread_id"`
	AuthorType string `json:"author_type"`
	AuthorName string `json:"author_name"`
	Content    string `json:"content"`
	CreatedAt  string `json:"created_at"`
	EditedAt   string `json:"edited_at"`
	Revision   int64  `json:"revision"`
}
