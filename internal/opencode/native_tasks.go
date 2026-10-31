package opencode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// These task sections follow Multica v0.7.2 prompt.go and execenv/channel_type.go.
func autopilotTask(t multica.Task) bool { return t.AutopilotRunID != "" }
func validAutopilot(t multica.Task) bool {
	return multica.ValidID(t.AutopilotRunID) && (t.AutopilotID == "" || multica.ValidID(t.AutopilotID)) &&
		t.ChatSessionID == "" && t.IssueID == "" && len(t.AutopilotTriggerPayload) <= 256<<10 &&
		(len(t.AutopilotTriggerPayload) == 0 || json.Valid(t.AutopilotTriggerPayload))
}

func autopilotPrompt(task multica.Task) string {
	var b strings.Builder
	b.WriteString("You are running as a local coding agent for a Multica workspace.\n\n")
	b.WriteString("This task was triggered by an Autopilot in run-only mode. There is no assigned Multica issue for this run.\n\n")
	fmt.Fprintf(&b, "Autopilot run ID: %s\n", task.AutopilotRunID)
	if task.AutopilotID != "" {
		fmt.Fprintf(&b, "Autopilot ID: %s\n", task.AutopilotID)
	}
	if task.AutopilotTitle != "" {
		fmt.Fprintf(&b, "Autopilot title: %s\n", task.AutopilotTitle)
	}
	if task.AutopilotSource != "" {
		fmt.Fprintf(&b, "Trigger source: %s\n", task.AutopilotSource)
	}
	// Keep the full bounded payload: the CLI cannot reread it later.
	if payload := strings.TrimSpace(string(task.AutopilotTriggerPayload)); payload != "" {
		fmt.Fprintf(&b, "Trigger payload:\n%s\n", payload)
	}
	b.WriteString("\nAutopilot instructions:\n")
	if strings.TrimSpace(task.AutopilotDescription) != "" {
		b.WriteString(task.AutopilotDescription)
		b.WriteString("\n\n")
	} else if task.AutopilotTitle != "" {
		fmt.Fprintf(&b, "%s\n\n", task.AutopilotTitle)
	} else {
		b.WriteString("No additional autopilot instructions were provided. Inspect the autopilot configuration before proceeding.\n\n")
	}
	b.WriteString("Complete the instructions above. Autopilot configuration is managed outside this task runtime.\n")
	// The issue-command boundary (execenv.AutopilotIssueCommandsGuard) is NOT
	// restated here: the brief's autopilot workflow section is its single
	// emission point, and a second hand-maintained per-turn copy is exactly
	// how the two surfaces drifted into conflict before (MUL-5696).
	return b.String()
}

func autopilotWorkflow(b *strings.Builder) {
	b.WriteString("**This task was triggered by an Autopilot in run-only mode.** There is no assigned Multica issue for this run.\n\n")
	b.WriteString("- The per-turn user message carries this run's autopilot instructions and its identifiers. Complete those instructions directly.\n")
	b.WriteString("- Do not run `multica issue get`, `multica issue comment add`, or `multica issue status` for this run unless the autopilot instructions explicitly tell you to create or update an issue\n\n")
}
func autopilotOutput(b *strings.Builder) {
	b.WriteString("## Output\n\nThis is a run-only autopilot task, so there may be no issue comment to post. Your final assistant output is captured automatically as the autopilot run result. Keep it concise and state the outcome.\n\n")
	b.WriteString("**Delivering files here:** this surface is text-only — the run result carries no attachments. Describe what you produced; do not link its path.\n")
}

func surfacePersistsTranscript(channel string) bool {
	switch channel {
	case "", "feishu", "wecom", "dingtalk":
		return true
	}
	return false
}
func channelDisplayName(channel string) string {
	switch channel {
	case "slack":
		return "Slack"
	case "feishu":
		return "Feishu/Lark"
	case "wecom":
		return "WeCom"
	case "dingtalk":
		return "DingTalk"
	case "telegram":
		return "Telegram"
	}
	return channel
}
func channelContext(task multica.Task) string {
	var b strings.Builder
	if task.ChatChannelType != "" {
		platform := channelDisplayName(task.ChatChannelType)
		fmt.Fprintf(&b, "You are operating inside a %s conversation — not the Multica web app. Never look in Multica issues or comments for this conversation.\n", platform)
		if task.ChatChannelType == "slack" {
			fmt.Fprintf(&b, "This conversation and its history live in %s, NOT in Multica. The message below may be only what triggered you. Read the conversation with:\n", platform)
			b.WriteString("- `multica chat history --output json` — the channel overview: recent top-level messages, each thread tagged with a `thread_id` and `reply_count`. It does NOT expand thread contents.\n")
			b.WriteString("- `multica chat thread [<thread_id>] --output json` — read one thread's messages; omit the id to read the thread you are in, or pass a `thread_id` from the overview to read a specific thread.\n")
			if task.ChatInThread {
				b.WriteString("You were @mentioned inside a thread: start with `multica chat thread` to read it; if you need the wider channel, run `multica chat history` and open a specific thread with `multica chat thread <thread_id>`.\n")
			} else {
				b.WriteString("You were @mentioned at the channel top level: start with `multica chat history` to see the channel, then read a specific thread's contents with `multica chat thread <thread_id>`.\n")
			}
			// These reads are the agent's private context-gathering; narrating them
			// into a chat reply reads as noise (the user reported every reply being
			// prefixed with "我先读取…"). Tell the agent to keep them out of its answer.
			b.WriteString("Do these reads SILENTLY as an internal step — they are how you gather context, not part of your answer.\n")
		} else if surfacePersistsTranscript(task.ChatChannelType) {
			fmt.Fprintf(&b, "The conversation happens in %s, and Multica stores a transcript of it. The message below may be only what triggered you — read it back with `multica chat history` when you need earlier context that is not below.\n", platform)
		} else {
			fmt.Fprintf(&b, "This conversation and its history live in %s, NOT in Multica, and Multica has no history reader for it. Work from the context already provided to you below — no command can fetch more of this conversation. If you genuinely need earlier context that is not here, ask the user for it rather than guessing.\n", platform)
		}
		// Scoped to process, not results — a completion confirmation IS the deliverable.
		fmt.Fprintf(&b, "Reply to %s with the final outcome only. Do NOT narrate planned or in-progress steps (\"我先读取…\"); completed actions are part of the outcome.\n", platform)
		b.WriteString("\n")
	}
	return b.String()
}
func chatNoticeFor(t multica.Task) string {
	if t.ChatChannelType == "slack" {
		return lostSession + "The channel conversation itself is unaffected — read it back with `multica chat history` / `multica chat thread` before acting, and treat what you find there as the authoritative version. " + lostMemory
	}
	if surfacePersistsTranscript(t.ChatChannelType) {
		return chatNotice()
	}
	return "## Session Continuity Notice\n\nThis run was meant to continue an earlier conversation, but that session's context could NOT be restored — you are starting fresh with no memory of the previous turns. That history is not readable from anywhere now: there is no command that fetches it, and only the context already in this message survives. **When you reply, tell the user up front (one short sentence) that the previous conversation context was unavailable and this is a new session**, so they understand why the thread did not carry over.\n\n"
}
func chatOutputFor(b *strings.Builder, t multica.Task) {
	if t.ChatChannelType == "" {
		chatOutput(b)
		return
	}
	b.WriteString("## Output\n\nThis is a chat session. Your reply is delivered directly to the chat window the user is reading.\n\n")
	fmt.Fprintf(b, "**Delivering files here:** whether Multica can push a file you produce into this %s conversation depends on how this deployment is configured, so it is stated per turn rather than here: the per-turn user message tells you, every turn. Follow what it says about files, and never report a file as delivered unless it told you how to deliver one.\n", channelDisplayName(t.ChatChannelType))
}
func turnContext(t multica.Task) string {
	var b strings.Builder
	if name := strings.NewReplacer("\r", " ", "\n", " ", "*", "").Replace(t.InitiatorName); name != "" {
		b.WriteString("\n## On Behalf Of\n\n")
		fmt.Fprintf(&b, "You are acting on behalf of **%s**. Apply any person-specific privacy or access rules in your instructions to this person. Your Multica credentials and access remain scoped to the runtime owner; do not assume this person can access everything you can.\n\n", name)
	}
	if strings.TrimSpace(t.WakeupJoined) != "" {
		b.WriteString("\n[WAKEUP — joined this run]\n" + strings.TrimSpace(t.WakeupJoined) + "\n\n")
	}
	if t.ProjectWorkflow != nil && strings.TrimSpace(t.ProjectWorkflow.Brief) != "" {
		b.WriteString("\n[Project workflow]\n" + strings.TrimSpace(t.ProjectWorkflow.Brief) + "\n\n")
	}
	return b.String()
}

const editedCommentGuidance = "The text quoted in this prompt is the current version and the only one to act on; treat any earlier wording, and plans that depended on it, as withdrawn. Work already done in this session (file changes, commands, posted comments) still happened and was not rolled back — re-evaluate what remains against the current text instead of repeating it or assuming it was undone."

func editedTriggerNote(t multica.Task) string {
	var b strings.Builder
	b.WriteString("This comment")
	if t.TriggerCommentID != "" {
		fmt.Fprintf(&b, " (%s", t.TriggerCommentID)
		if t.TriggerCommentRevision > 0 {
			fmt.Fprintf(&b, ", revision %d", t.TriggerCommentRevision)
		}
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " was edited at %s. Your session history may still contain an earlier version of it. %s\n\n", t.TriggerCommentEditedAt, editedCommentGuidance)
	return b.String()
}

// wakeupPrompt is the native issue continuation, whose trigger is a fact, not completion.
func wakeupPrompt(task multica.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are running as a local coding agent for a Multica workspace.\n\nYour assigned issue ID is: %s\n\n[WAKEUP]\n%s\n\n", task.IssueID, task.HandoffNote)
	fmt.Fprintf(&b, "Start by running `multica issue get %s --output json`, then read current run/comment state. Decide whether the instruction's goal is met; the trigger reports a fact, not business completion. This is an ordinary run with normal result delivery, except where the [WAKEUP] block offers a check-in.\n", task.IssueID)
	fmt.Fprintf(&b, "Scan comment threads with `multica issue comment list %s --roots-only --summary --compact --output json`, then expand relevant threads with `--thread <id> --tail 30`.\n", task.IssueID)
	if task.WakeupSystemRule != "" {
		// Platform rules belong to the issue, not to a run; members manage them.
		fmt.Fprintf(&b, "This wakeup is the platform's sub-issue rule for this issue. Do not try to change or disable it; members manage it on the issue.\n")
	} else {
		fmt.Fprintf(&b, "Inspect this configuration with `multica issue wakeup get %s %s --output json`. If recurring work is no longer needed, disable it with `multica issue wakeup disable %s %s`.\n", task.IssueID, task.WakeupID, task.IssueID, task.WakeupID)
	}
	if task.TriggerCommentID != "" {
		fmt.Fprintf(&b, "Post your result using `multica issue comment add %s --parent %s --content-file ./reply.md --output table && rm ./reply.md`. This is the original delivery thread, not a new comment trigger.\n", task.IssueID, task.TriggerCommentID)
	}
	return b.String()
}
