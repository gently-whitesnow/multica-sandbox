package opencode

import (
	"fmt"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Text below mirrors Multica v0.7.2 server/internal/daemon/prompt.go (buildChatPrompt,
// sessionContinuityNoticeFor) and execenv/{channel_type,runtime_config_sections}.go chat
// sections for web chat, with the explicit skill selection of slash links (ADR 0017).

// chatTask follows upstream: channel and web chat use the same retained conversation.
func chatTask(t multica.Task) bool { return t.ChatSessionID != "" }

// validChat bounds chat claims; shapes the server would not send reject the claim.
func validChat(t multica.Task) bool {
	if !multica.ValidID(t.ChatSessionID) || len(t.ChatMessageAttachments) > 64 || len(t.ChatChannelType) > 64 || strings.ContainsAny(t.ChatChannelType, "\x00\r\n") {
		return false
	}
	for _, a := range t.ChatMessageAttachments {
		if !multica.ValidID(a.ID) || len(a.Filename) > 1024 || len(a.ContentType) > 255 || strings.ContainsAny(a.ContentType, "\x00\r\n") {
			return false
		}
	}
	return true
}

// audience is upstream AudienceOf; a room shape the server does not send reads as unknown.
func audience(t multica.Task) string {
	switch {
	case t.ChatType == "group":
		return "Audience: group room; not private; unseen members may read replies.\n\n"
	case t.ChatType == "p2p" || t.ChatType == "" && t.ChatChannelType == "":
		return "Audience: direct room.\n\n"
	}
	return "Audience: unknown.\n\n"
}

// chatPrompt renders upstream buildChatPrompt.
func chatPrompt(t multica.Task) string {
	var b strings.Builder
	b.WriteString("You are running as a chat assistant for a Multica workspace.\n")
	if t.ChatIntro {
		b.WriteString("You were just created, and this is the very first message in a direct chat with the person who created you. They have not written anything yet — you are opening the conversation. Send a short, warm, first-person introduction: who you are, what you're good at, and how they can work with you. Do NOT phrase it as an answer to a question or repeat any prompt back; just introduce yourself as if you reached out first.\n")
		return b.String()
	}
	b.WriteString(audience(t))
	b.WriteString(channelContext(t))
	if selected := selectedSkills(t); len(selected) > 0 {
		b.WriteString("Explicitly selected skills:\n")
		for _, name := range selected {
			fmt.Fprintf(&b, "- %s\n", name)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "User message:\n%s\n", t.ChatMessage)
	if len(t.ChatMessageAttachments) > 0 {
		b.WriteString("\nAttachments on this message:\n")
		for _, a := range t.ChatMessageAttachments {
			if a.ContentType != "" {
				fmt.Fprintf(&b, "- id=%s filename=%q content_type=%s\n", a.ID, a.Filename, a.ContentType)
			} else {
				fmt.Fprintf(&b, "- id=%s filename=%q\n", a.ID, a.Filename)
			}
		}
		if t.ChatChannelType == "" {
			b.WriteString("Use `multica attachment download <id>` to fetch each file locally before referring to it.\n")
			b.WriteString("When creating an issue that should preserve one of these attachments, pass `--attachment-id <id>` to `multica issue create` in addition to keeping the attachment markdown inline.\n")
		} else {
			b.WriteString("Attachment contents cannot be downloaded by this sandbox. Work from the text provided; ask for essential attachment text when needed.\n")
		}
	}
	switch {
	case t.ChatChannelType == "":
		b.WriteString("\nTo include a file or image you produced in your reply, run `multica attachment upload <local-path>`. The file binds to your reply automatically and appears as an attachment card below it even if you paste nothing. The command also returns a `markdown` snippet you may paste on its own line to place the item where you want it (files render as a card, images inline).\n")
	default:
		fmt.Fprintf(&b, "\nThis sandbox delivers replies to %s as text only. File upload/download and attachment delivery are unavailable in this runtime, even if the channel itself supports files. If you produce a file, describe it in words — never write its local path as a link or claim it was delivered.\n", channelDisplayName(t.ChatChannelType))
	}

	return b.String()
}

const (
	lostSession = "## Session Continuity Notice\n\nThis run was meant to continue an earlier conversation, but that provider session could not be restored, and this run does not continue it. "
	lostMemory  = "What is gone is your own working memory from the turns that did not come back: what you already tried, what you ruled out, and how far you had got. Re-derive what you need instead of assuming it. Do not open your reply by announcing this — raise it only where it actually matters.\n\n"
)

// chatNotice is upstream SessionContinuityNoticeChatTranscript: web chat keeps its transcript.
func chatNotice() string {
	return lostSession + "The conversation itself is unaffected — Multica stored it, and you can read it back with `multica chat history` before acting; treat what you find there as the authoritative version. " + lostMemory
}

// chatWorkflow is upstream writeWorkflowChat.
func chatWorkflow(b *strings.Builder) {
	b.WriteString("**You are in chat mode.**\n\n")
	b.WriteString("- Respond conversationally and helpfully to the user's message\n")
	b.WriteString("- Use the task-scoped `multica` CLI to look up issues, workspace info, members and agents; administration remains outside this runtime.\n")
	b.WriteString("- If asked about issues, use `multica issue list --output json` or `multica issue get <id> --output json`\n")
	b.WriteString("- If asked about the workspace, use `multica workspace get --output json`\n")
	b.WriteString("- If asked to perform actions (create issues, update status, etc.), use the appropriate CLI commands\n")
	b.WriteString("- If the task requires code changes, use `multica repo checkout <url>` to get the code first. Use `--ref <branch-or-sha>` when you need an exact revision\n")
	b.WriteString("- Keep responses concise and direct\n\n")
}

// chatOutput is upstream writeOutput for chat with its delivery invariant.
func chatOutput(b *strings.Builder) {
	b.WriteString("## Output\n\n")
	b.WriteString("This is a chat session. Your reply is delivered directly to the chat window the user is reading.\n\n")
	b.WriteString("**Delivering files here:** run `multica attachment upload <local-path>` — it binds the file to your reply and it renders as an attachment card. That command is the ONLY way a file reaches the user; a path written into your reply text is not.\n")
	b.WriteString("\n**Runtime-local paths are never deliverables.** Your working directory exists only on the machine running you — NEVER write an absolute path or a `file://` URL as a clickable link or an embedded image. Reference code locations as inline code, never a link: `path/to/file.ts:42`. Deliver files through this surface's mechanism (above); if it has none, say so in words — never link the path and imply the file was delivered.\n")
}
