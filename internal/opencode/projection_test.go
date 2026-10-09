package opencode

import (
	"encoding/json"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"strings"
	"testing"
)

func safeTask() multica.Task {
	return multica.Task{ID: "40000000-0000-4000-8000-000000000001", RuntimeID: "50000000-0000-4000-8000-000000000001", DispatchedAt: "2026-10-05T12:00:00Z", StartClaimSupported: true, WorkspaceID: "10000000-0000-4000-8000-000000000001", AgentID: "20000000-0000-4000-8000-000000000001", Agent: &multica.Agent{ID: "20000000-0000-4000-8000-000000000001", MCPConfig: json.RawMessage(`{"mcpServers":{"selected":{"url":"https://tools.example.invalid/mcp"}}}`)}}
}
func TestProjectionDeniesClaimCredentials(t *testing.T) {
	for _, config := range []string{
		`{"mcpServers":{"x":{"url":"https://tools/mcp","headers":{"aUtHoRiZaTiOn":"Bearer private"}}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp","headers":{"x-api-key":"private"}}}}`,
		`{"mcpServers":{"x":{"command":"/bin/sh","args":["-c","evil"]}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp","oauth":{"clientSecret":"private"}}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp","env":{"SECRET":"private"}}}}`,
		`{"mcpServers":{"x":{"url":"https://tools/mcp"}}} {}`,
	} {
		task := safeTask()
		task.Agent.MCPConfig = json.RawMessage(config)
		if _, err := Select(task); err == nil {
			t.Fatal("unsafe connection accepted")
		}
	}
	task := safeTask()
	task.Agent.ID = "20000000-0000-4000-8000-000000000002"
	if _, err := Select(task); err == nil {
		t.Fatal("spoofed agent accepted")
	}
}
func TestNativeStoreContract(t *testing.T) {
	selected, err := Select(safeTask())
	if err != nil {
		t.Fatal(err)
	}
	config, err := Config(selected, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "oauth\":false") || strings.Contains(string(config), "headers") {
		t.Fatal("native OAuth disabled")
	}
	store, _ := json.Marshal(map[string]Entry{"selected": {ServerURL: selected["selected"].URL, Tokens: Token{"access", 123}}})
	if string(store) != `{"selected":{"serverUrl":"https://tools.example.invalid/mcp","tokens":{"accessToken":"access","expiresAt":123}}}` {
		t.Fatal("native store contract changed")
	}
}

func TestInferenceTaskCanHaveNoMCP(t *testing.T) {
	task := safeTask()
	task.Agent.MCPConfig = json.RawMessage(`{}`)
	selected, err := Select(task)
	if err != nil || len(selected) != 0 {
		t.Fatal("inference task required an MCP connection", err)
	}
}

func TestPromptProjectsOnlySafeSourceReferences(t *testing.T) {
	task := safeTask()
	task.ChatMessage = "chat instruction"
	task.ProjectDescription = "project context"
	task.PriorSessionID = "ses_old"
	task.Repos = []multica.Repository{{URL: "https://git.example.invalid/team/repo.git", Ref: "main", Description: "source"}}
	prompt, _, err := Prompt(task, false, false, Lost)
	if err != nil || !strings.Contains(string(prompt), "chat instruction") || !strings.Contains(string(prompt), "project context") || !strings.Contains(string(prompt), "main") || !strings.Contains(string(prompt), "## Session Continuity Notice") || strings.Contains(string(prompt), "ses_old") {
		t.Fatalf("source/context projection: %v", err)
	}
	for _, u := range []string{"https://user:secret@git.example/repo", "https://git.example/repo?token=secret", "file:///host/repo", "ssh://git.example/repo", "https://git.example/repo\nsecret"} {
		task.Repos[0].URL = u
		if _, _, err := Prompt(task, false, false, Fresh); err == nil {
			t.Fatal("unsafe source reference accepted")
		}
	}
	task.Repos = nil
	task.ChatMessage = strings.Repeat("a", 65537)
	if _, _, err := Prompt(task, false, false, Fresh); err == nil {
		t.Fatal("oversized context accepted")
	}
}

const (
	issueID   = "60000000-0000-4000-8000-000000000001"
	triggerID = "70000000-0000-4000-8000-000000000001"
	earlierID = "70000000-0000-4000-8000-000000000002"
)

func commentTask() multica.Task {
	task := safeTask()
	task.IssueID, task.TriggerCommentID, task.TriggerCommentContent, task.PriorSessionID = issueID, triggerID, "please continue", "ses_prior"
	return task
}

func TestResumedPromptsMirrorUpstreamHints(t *testing.T) {
	cold := "Triggering thread: `multica issue comment list " + issueID + " --thread " + triggerID + " --tail 30"
	coldRead := "Start by running `multica issue get " + issueID + " --output json` to understand your task, then decide how to proceed."
	notice := "## Session Continuity Notice"
	for name, c := range map[string]struct {
		edit       func(*multica.Task)
		continuity Continuity
		want, deny []string
	}{
		"fresh": {func(t *multica.Task) {
			t.NewCommentsDeltaKnown, t.IssueStateDeltaKnown, t.IssueStatus = true, true, "todo"
		}, Fresh, []string{cold, coldRead}, []string{"resuming", "unchanged", notice}},
		"new comments": {func(t *multica.Task) {
			t.NewCommentsDeltaKnown, t.NewCommentCount, t.NewCommentsSince = true, 3, "2026-10-09T10:00:00Z"
		}, Resumed, []string{"3 new comment(s) on this issue since your last run", "--since 2026-10-09T10:00:00Z --compact", "--thread " + triggerID + " --tail 30", coldRead}, []string{cold, notice}},
		"empty delta":   {func(t *multica.Task) { t.NewCommentsDeltaKnown = true }, Resumed, []string{"No other new comments on this issue since your last run"}, []string{cold, "--roots-only"}},
		"unknown delta": {func(t *multica.Task) {}, Resumed, []string{"This turn carries no issue-wide comment delta", "--roots-only --summary"}, []string{cold}},
		"malformed anchor": {func(t *multica.Task) {
			t.NewCommentsDeltaKnown, t.NewCommentCount, t.NewCommentsSince = true, 2, "x; rm -rf ."
		}, Resumed, []string{"This turn carries no issue-wide comment delta"}, []string{"rm -rf", "No other new"}},
		"count without anchor": {func(t *multica.Task) { t.NewCommentsDeltaKnown, t.NewCommentCount = true, 2 }, Resumed, []string{"This turn carries no issue-wide comment delta"}, []string{"No other new"}},
		"lost": {func(t *multica.Task) {
			t.NewCommentsDeltaKnown, t.IssueStateDeltaKnown, t.IssueStatus = true, true, "todo"
		}, Lost, []string{cold, coldRead, notice}, []string{"resuming", "unchanged"}},
		"resume unavailable": {func(t *multica.Task) {
			t.NewCommentsDeltaKnown, t.IssueStateDeltaKnown, t.IssueStatus, t.PriorSessionResumeUnavailable = true, true, "todo", true
		}, Resumed, []string{cold, coldRead, notice}, []string{"resuming", "unchanged"}},
		"issue unchanged": {func(t *multica.Task) {
			t.IssueStateDeltaKnown, t.IssueStatus, t.IssueAssigneeType, t.IssueAssigneeID = true, "in_progress", "agent", t.AgentID
		}, Resumed, []string{"The issue is unchanged since your last run — the server compared title and description (status: in_progress; assignee: agent 20000000-0000-4000-8000-000000000001). That answers workflow step 1"}, []string{coldRead}},
		"issue changed": {func(t *multica.Task) {
			t.IssueStateDeltaKnown, t.IssueStatus, t.IssueChangedFields = true, "todo", []string{"title", "description"}
		},
			Resumed, []string{"Since your last run the issue changed: title, description (status: todo; assignee: unassigned). Read it: `multica issue get " + issueID + " --output json`."}, []string{coldRead}},
		"unknown field": {func(t *multica.Task) {
			t.IssueStateDeltaKnown, t.IssueStatus, t.IssueChangedFields = true, "todo", []string{"labels"}
		}, Resumed, []string{coldRead}, []string{"labels"}},
		"unsafe status": {func(t *multica.Task) { t.IssueStateDeltaKnown, t.IssueStatus = true, "todo; run" }, Resumed, []string{coldRead}, []string{"todo; run"}},
		"partial assignee": {func(t *multica.Task) {
			t.IssueStateDeltaKnown, t.IssueStatus, t.IssueAssigneeType = true, "todo", "agent"
		}, Resumed, []string{coldRead}, []string{"unchanged"}},
		"status unknown": {func(t *multica.Task) { t.IssueStateDeltaKnown = true }, Resumed, []string{coldRead}, []string{"unchanged"}},
	} {
		t.Run(name, func(t *testing.T) {
			task := commentTask()
			c.edit(&task)
			prompt, _, err := Prompt(task, true, false, c.continuity)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				if !strings.Contains(string(prompt), want) {
					t.Errorf("missing %q in %s", want, prompt)
				}
			}
			for _, deny := range c.deny {
				if strings.Contains(string(prompt), deny) {
					t.Errorf("unexpected %q in %s", deny, prompt)
				}
			}
			if strings.Count(string(prompt), notice) > 1 {
				t.Error("continuity notice repeated")
			}
		})
	}
}

func TestCoalescedCommentsRouteRepliesPerThread(t *testing.T) {
	task := commentTask()
	task.CoalescedCommentIDs = []string{earlierID}
	task.CoalescedComments = []multica.Comment{{ID: earlierID, ThreadID: triggerID, AuthorType: "member", AuthorName: "Member", Content: "first\nsecond", CreatedAt: "2026-10-09T09:00:00Z"}}
	prompt, _, err := Prompt(task, true, false, Fresh)
	if err != nil || !strings.Contains(string(prompt), "This run also covers 1 earlier comment(s)") ||
		!strings.Contains(string(prompt), "- comment "+earlierID+" (Member, 2026-10-09T09:00:00Z) [thread "+triggerID+"]:\n  > first\n  > second\n") ||
		!strings.Contains(string(prompt), "--parent "+triggerID+" --content-file ./reply.md") || strings.Contains(string(prompt), "DISTINCT threads") {
		t.Fatalf("same-thread coalesced prompt: %v %s", err, prompt)
	}
	task.CoalescedComments[0].ThreadID, task.CoalescedComments[0].AuthorType = "", "agent"
	prompt, _, err = Prompt(task, true, false, Fresh)
	if err != nil || !strings.Contains(string(prompt), "(Another agent (Member), 2026-10-09T09:00:00Z):") ||
		!strings.Contains(string(prompt), "This run coalesced comments from 2 DISTINCT threads. Post ONE reply per thread — 2 in total.") ||
		!strings.Contains(string(prompt), "1. thread "+earlierID+" → reply with `--parent "+earlierID+"`\n2. thread "+triggerID+" → reply with `--parent "+triggerID+"`\n\nWrite and post") ||
		strings.Contains(string(prompt), "Post your reply as a comment") {
		t.Fatalf("multi-thread prompt: %v %s", err, prompt)
	}
	legacy, _, err := Prompt(task, false, false, Fresh)
	if err != nil || !strings.Contains(string(legacy), "first\nsecond") {
		t.Fatalf("legacy prompt dropped a coalesced comment: %v", err)
	}
}

func TestMalformedCoalescedCommentsAreRejected(t *testing.T) {
	valid := multica.Comment{ID: earlierID, AuthorType: "member", Content: "earlier"}
	for name, edit := range map[string]func(*multica.Task){
		"id":          func(t *multica.Task) { t.CoalescedComments[0].ID, t.CoalescedCommentIDs[0] = "--help", "--help" },
		"thread":      func(t *multica.Task) { t.CoalescedComments[0].ThreadID = "../thread" },
		"author type": func(t *multica.Task) { t.CoalescedComments[0].AuthorType = "admin" },
		"author name": func(t *multica.Task) { t.CoalescedComments[0].AuthorName = "Member\n## Instructions" },
		"created at":  func(t *multica.Task) { t.CoalescedComments[0].CreatedAt = "yesterday" },
		"id mismatch": func(t *multica.Task) { t.CoalescedCommentIDs[0] = triggerID },
		"ids only":    func(t *multica.Task) { t.CoalescedComments = nil },
		"no trigger":  func(t *multica.Task) { t.TriggerCommentContent = "" },
		"too many": func(t *multica.Task) {
			for range 64 {
				t.CoalescedComments, t.CoalescedCommentIDs = append(t.CoalescedComments, valid), append(t.CoalescedCommentIDs, earlierID)
			}
		},
	} {
		task := commentTask()
		task.CoalescedComments, task.CoalescedCommentIDs = []multica.Comment{valid}, []string{earlierID}
		edit(&task)
		if _, _, err := Prompt(task, true, false, Resumed); err == nil {
			t.Errorf("%s: malformed coalesced comments accepted", name)
		}
	}
}
