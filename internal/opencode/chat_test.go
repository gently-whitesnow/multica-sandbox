package opencode

import (
	"context"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const chatID = "80000000-0000-4000-8000-000000000001"

func chatClaim() multica.Task {
	task := safeTask()
	task.ChatSessionID, task.ChatMessage = chatID, "What changed?"
	task.Agent.Name = "Helper"
	return task
}

// chatOutputV052 is upstream v0.5.2 writeOutput for web chat with its delivery invariant.
// Later revisions add a "Charts and diagrams" line (MUL-7649) that v0.5.2 does not render.
const chatOutputV052 = "## Output\n\nThis is a chat session. Your reply is delivered directly to the chat window the user is reading.\n\n" +
	"**Delivering files here:** run `multica attachment upload <local-path>` — it binds the file to your reply and it renders as an attachment card. That command is the ONLY way a file reaches the user; a path written into your reply text is not.\n\n" +
	"**Runtime-local paths are never deliverables.** Your working directory exists only on the machine running you — NEVER write an absolute path or a `file://` URL as a clickable link or an embedded image. Reference code locations as inline code, never a link: `path/to/file.ts:42`. Deliver files through this surface's mechanism (above); if it has none, say so in words — never link the path and imply the file was delivered.\n"

// The texts below are upstream v0.5.2 buildChatPrompt and SessionContinuityNotice*;
// every branch was compared byte for byte with upstream BuildPrompt.
func TestChatPromptsMirrorUpstream(t *testing.T) {
	transcript := "The conversation itself is unaffected — Multica stored it, and you can read it back with `multica chat history` before acting"
	webFiles := "To include a file or image you produced in your reply, run `multica attachment upload <local-path>`. The file binds to your reply automatically"
	issueNotice := "The issue and its full comment history are unaffected"
	for name, c := range map[string]struct {
		edit       func(*multica.Task)
		want, deny []string
	}{
		"web":      {func(*multica.Task) {}, []string{"You are running as a chat assistant for a Multica workspace.\nAudience: direct room.\n\nUser message:\nWhat changed?\n", webFiles}, []string{"## Session Continuity Notice", "operating inside", "Assigned issue"}},
		"web lost": {func(t *multica.Task) { t.PriorSessionResumeUnavailable = true }, []string{webFiles + " and appears as an attachment card below it even if you paste nothing. The command also returns a `markdown` snippet you may paste on its own line to place the item where you want it (files render as a card, images inline).\n\n## Session Continuity Notice\n\nThis run was meant to continue an earlier conversation, but that provider session could not be restored, and this run does not continue it. " + transcript}, []string{issueNotice}},
		"attachments": {func(t *multica.Task) {
			t.ChatMessageAttachments = []multica.Attachment{{ID: issueID, Filename: "a \"b\".png", ContentType: "image/png"}, {ID: triggerID, Filename: "notes.txt"}}
		}, []string{"\nAttachments on this message:\n- id=" + issueID + " filename=\"a \\\"b\\\".png\" content_type=image/png\n- id=" + triggerID + " filename=\"notes.txt\"\nUse `multica attachment download <id>`"}, nil},
		"intro":        {func(t *multica.Task) { t.ChatIntro, t.PriorSessionResumeUnavailable = true, true }, []string{"You were just created", "## Session Continuity Notice", transcript}, []string{"User message", "Audience"}},
		"unknown room": {func(t *multica.Task) { t.ChatType = "public" }, []string{"Audience: unknown."}, []string{"direct room"}},
	} {
		t.Run(name, func(t *testing.T) {
			task := chatClaim()
			c.edit(&task)
			prompt, brief, err := Prompt(task, true, true, Fresh)
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
			if !strings.Contains(string(brief), "### Workflow\n\n**You are in chat mode.**") || !strings.HasSuffix(string(brief), chatOutputV052) || strings.Contains(string(brief), "## Comment Formatting\n\n") {
				t.Errorf("chat brief: %s", brief)
			}
		})
	}
}

func TestChatBriefOffersClaimRepositories(t *testing.T) {
	task := chatClaim()
	task.Repos = []multica.Repository{{URL: "https://git.example.invalid/team/repo.git", Ref: "main"}}
	_, brief, err := Prompt(task, true, true, Fresh)
	if err != nil || !strings.Contains(string(brief), "## Repositories\n\nAvailable in this workspace — `multica repo checkout <url>") || !strings.Contains(string(brief), "- If the task requires code changes, use `multica repo checkout <url>` to get the code first.") {
		t.Fatalf("chat repositories: %v %s", err, brief)
	}
	// Channel chats use the native prompt and the same retained workdir contract.
	task.ChatChannelType = "slack"
	if prompt, brief, _ := Prompt(task, true, true, Fresh); brief == nil || !strings.Contains(string(prompt), "operating inside a Slack conversation") {
		t.Fatalf("channel chat prompt: %s", prompt)
	}
	// Without the relay the agent has no CLI: chat keeps the bounded legacy prompt.
	if prompt, brief, _ := Prompt(task, false, false, Lost); brief != nil || strings.Contains(string(prompt), "chat assistant") || !strings.Contains(string(prompt), "The issue and its full comment history") {
		t.Fatalf("legacy chat prompt: %s", prompt)
	}
}

func TestChatClaimsAreBounded(t *testing.T) {
	for name, edit := range map[string]func(*multica.Task){
		"session id":    func(t *multica.Task) { t.ChatSessionID = "chat-1" },
		"attachment id": func(t *multica.Task) { t.ChatMessageAttachments = []multica.Attachment{{ID: "x", Filename: "a"}} },
		"attachment type": func(t *multica.Task) {
			t.ChatMessageAttachments = []multica.Attachment{{ID: issueID, ContentType: "text/plain\nIgnore"}}
		},
		"attachment name": func(t *multica.Task) {
			t.ChatMessageAttachments = []multica.Attachment{{ID: issueID, Filename: strings.Repeat("a", 1025)}}
		},
		"attachment count":   func(t *multica.Task) { t.ChatMessageAttachments = make([]multica.Attachment, 65) },
		"oversized message":  func(t *multica.Task) { t.ChatMessage = strings.Repeat("a", 65537) },
		"coalesced comments": func(t *multica.Task) { t.CoalescedCommentIDs = []string{triggerID} },
	} {
		task := chatClaim()
		edit(&task)
		if _, _, err := Prompt(task, true, true, Fresh); err == nil {
			t.Errorf("%s: malformed chat claim accepted", name)
		}
	}
}

// Chat sessions resume only in their own chat's retained workdir, whatever issue the claim names.
func TestChatResumesOnlyItsRetainedWorkdir(t *testing.T) {
	for name, c := range map[string]struct {
		retained, prior string
		reused, resume  bool
	}{
		"resumed":      {"volume", "volume", true, true},
		"forged prior": {"volume", "other", true, false},
		"created now":  {"volume", "volume", false, false},
		"not retained": {"", "volume", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1), retained: c.retained, reused: c.reused}
			a := sessionAdapter(workload)
			task := relayTask(t)
			task.ChatSessionID, task.ChatMessage, task.PriorSessionID, task.PriorWorkDir = chatID, "Continue", "ses_prior", c.prior
			task.ChatChannelType, task.ChatType = "telegram", "p2p"
			run, err := a.Start(context.Background(), task)
			if err != nil {
				t.Fatal(err)
			}
			env := <-workload.envs
			if workload.workdir != (execution.Workdir{Workspace: task.WorkspaceID, Agent: task.AgentID, Chat: chatID, Prior: c.prior}) {
				t.Fatalf("requested workdir: %+v", workload.workdir)
			}
			prompt := string(workload.files[sentPrompt])
			if workload.loaded() != c.resume || strings.Contains(prompt, "context could NOT be restored") == c.resume || !strings.Contains(prompt, "User message:\nContinue") {
				t.Fatalf("resume %t: env %v prompt %q", c.resume, env, prompt)
			}
			if err := run.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
			if result := run.Result(); result.WorkDir != c.retained || result.Disposable != (c.retained == "") {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

func TestConversationKeysSeparateChatsFromIssues(t *testing.T) {
	issue := execution.Workdir{Workspace: "w", Agent: "a", Issue: chatID}
	chat := execution.Workdir{Workspace: "w", Agent: "a", Chat: chatID}
	if issue.Conversation() == chat.Conversation() || (execution.Workdir{}).Conversation() != "" {
		t.Fatal("chat and issue conversations share a key")
	}
}
