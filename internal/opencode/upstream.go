package opencode

import (
	"fmt"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Text below mirrors Multica b4ca5b4a23e68b26292a680dca7689a952bb1cd5
// server/internal/daemon/prompt.go (buildPromptBody, buildCommentPrompt) and
// execenv/{runtime_config_sections,reply_instructions,issue_state_instructions}.go,
// reduced to the issue workflow this adapter supports. Re-check on upgrades.

const BriefPath = "/workspace/AGENTS.md"

// Environment mirrors upstream taskMulticaEnvironment without daemon ports or host paths.
func (r *running) environment() map[string]string {
	if r.relayToken == "" {
		return nil
	}
	env := map[string]string{
		"MULTICA_SERVER_URL":       r.adapter.RelayURL,
		"MULTICA_TOKEN":            r.relayToken,
		"MULTICA_WORKSPACE_ID":     r.task.WorkspaceID,
		"MULTICA_AGENT_ID":         r.task.AgentID,
		"MULTICA_TASK_ID":          r.task.ID,
		"MULTICA_TASK_CONFIG_ROOT": "/workspace/data/multica",
	}
	if name := r.task.Agent.Name; name != "" && len(name) <= 256 && !strings.ContainsAny(name, "\x00\r\n") {
		env["MULTICA_AGENT_NAME"] = name
	}
	return env
}

// upstreamTask reports whether the claim is an issue task the upstream CLI workflow covers.
func upstreamTask(t multica.Task) bool {
	return multica.ValidID(t.IssueID) && t.ChatMessage == "" &&
		(t.TriggerCommentID == "" || multica.ValidID(t.TriggerCommentID)) &&
		(t.TriggerThreadID == "" || multica.ValidID(t.TriggerThreadID))
}

func upstreamPrompt(t multica.Task) string {
	var b strings.Builder
	b.WriteString("You are running as a local coding agent for a Multica workspace.\n\n")
	fmt.Fprintf(&b, "Your assigned issue ID is: %s\n\n", t.IssueID)
	if t.TriggerCommentID == "" {
		fmt.Fprintf(&b, "Start by running `multica issue get %s --output json` to understand your task, then complete it.\n", t.IssueID)
		fmt.Fprintf(&b, "For comment history, workflow step 2 applies. Scan the threads first with `multica issue comment list %s --roots-only --summary --compact --output json`, then expand only what matters with `--thread <thread-id> --tail 30`. For `--since` incremental polling, pagination, and folding, see `multica issue comment list --help`.\n", t.IssueID)
		return b.String()
	}
	if t.TriggerCommentContent != "" {
		author := "A user"
		switch t.TriggerAuthorType {
		case "system":
			author = "The platform"
		case "agent":
			name := t.TriggerAuthorName
			if name == "" {
				name = "another agent"
			}
			author = fmt.Sprintf("Another agent (%s)", name)
		}
		fmt.Fprintf(&b, "[NEW COMMENT] %s just left a new comment. Focus on THIS comment — do not confuse it with previous ones:\n\n", author)
		fmt.Fprintf(&b, "> %s\n\n", t.TriggerCommentContent)
	}
	// Every sandbox attempt is a fresh session, so the cold hints always apply.
	fmt.Fprintf(&b, "Start by running `multica issue get %s --output json` to understand your task, then decide how to proceed.\n\n", t.IssueID)
	thread := t.TriggerThreadID
	if thread == "" {
		thread = t.TriggerCommentID
	}
	fmt.Fprintf(&b, "Triggering thread: `multica issue comment list %s --thread %s --tail 30 --compact --output json` (that thread's root + its 30 newest replies). The scan workflow step 2 requires is the same command with `--roots-only --summary` in place of `--thread ... --tail 30`.\n\n", t.IssueID, thread)
	fmt.Fprintf(&b, "Post your reply as a comment — always use the trigger comment ID below, do NOT reuse --parent values from previous turns in this session.\n\n"+
		"Write the body file first (rules: ## Comment Formatting above — MUL-2904 / #4182):\n\n"+
		"    multica issue comment add %s --parent %s --content-file ./reply.md --output table && rm ./reply.md\n\n"+
		"Keep the `&&`: as two separate statements a failed post is masked by the cleanup's success, and the body file is deleted.\n\n"+
		"Do NOT write literal `\\n` escapes to simulate line breaks; the file preserves real newlines.\n", t.IssueID, t.TriggerCommentID)
	return b.String()
}

// upstreamBrief is the runtime brief OpenCode reads from AGENTS.md in its working directory.
func upstreamBrief(t multica.Task, repositories string) string {
	var b strings.Builder
	b.WriteString("# Multica Agent Runtime\n\n")
	b.WriteString("You are a coding agent in the Multica platform. Use the `multica` CLI to interact with the platform.\n\n")
	if t.Agent.Instructions != "" || t.Agent.Name != "" {
		b.WriteString("## Agent Identity\n\n")
		if t.Agent.Name != "" {
			fmt.Fprintf(&b, "**You are: %s** (ID: `%s`)\n\n", strings.NewReplacer("\r", " ", "\n", " ", "*", "").Replace(t.Agent.Name), t.AgentID)
		}
		if t.Agent.Instructions != "" {
			b.WriteString(t.Agent.Instructions + "\n\n")
		}
	}
	if context := strings.TrimRight(t.WorkspaceContext, " \t\r\n"); context != "" {
		b.WriteString("## Workspace Context\n\n" + context + "\n\n")
	}
	b.WriteString("## Available Commands\n\n")
	b.WriteString("Prefer `--output json` for structured data. For everything else run `multica --help` or `multica <command> --help`.\n\n")
	b.WriteString("`--output json` writes JSON to stdout; confirmations and warnings go to stderr. Do not merge them (`2>&1`) into anything that parses the output — that makes a write that SUCCEEDED look like it failed and invites a duplicate retry.\n\n")
	b.WriteString("### Core\n")
	b.WriteString("- `multica issue get <id> --output json` — full issue.\n")
	b.WriteString("- `multica issue comment list <issue-id> [--roots-only] [--summary] [--thread <comment-id> [--tail N] | --recent N] [--since <RFC3339>] --output json` — thread-aware comment reads. Bound a wide read with `--roots-only --summary`; bound a deep one with `--thread <id> --tail N`; add `--compact` to any JSON read to drop echoed/null/bookkeeping fields.\n")
	b.WriteString("- `multica issue status <id> <status>` — flip status (todo / in_progress / in_review / done / blocked / backlog / cancelled).\n")
	b.WriteString("- `multica issue comment add <issue-id> [--content \"...\" | --content-file <path> | --content-stdin] [--parent <comment-id>] [--attachment <path>]` — post a comment. Agent-authored bodies MUST use `--content-file`; see `## Comment Formatting` for why.\n\n")
	b.WriteString("## Comment Formatting\n\n")
	b.WriteString("For issue comments, **always write the comment body to a UTF-8 file with your file-write tool first, then post it with `--content-file <path>`**. Never use inline `--content` for agent-authored comments; never use `--content-stdin` HEREDOCs alongside other flags. Write the file inside your working directory, never `/tmp` or shared paths. Keep the same `--parent` value from the trigger comment when replying; delete the temp file (`rm ./reply.md`) only after the post succeeded; do not rely on `\\n` escapes.\n\n")
	b.WriteString("For final-result comments, use `--output table` to confirm success without echoing the body. Use `--output json` instead when you need the returned comment ID. Gate the cleanup on the post succeeding (`&&`): a cleanup command run unconditionally succeeds after a failed post and makes the whole shell call exit 0.\n\n")
	if repositories != "" {
		b.WriteString("## Repositories\n\n" + repositories + "\n\n")
	}
	if t.ProjectTitle != "" || t.ProjectDescription != "" {
		b.WriteString("## Project Context\n\n" + strings.TrimSpace(t.ProjectTitle+"\n\n"+t.ProjectDescription) + "\n\n")
	}
	b.WriteString("### Workflow\n\n")
	b.WriteString("1. Read the issue (`multica issue get`) to understand the context.\n")
	b.WriteString("2. Catch up on the comment history — this is mandatory — in two bounded reads, never one bulk pull: scan every thread cheaply (`--roots-only --summary --compact`), then expand only the threads that matter (`--thread <id> --tail 30 --compact`).\n")
	b.WriteString("3. If any part of what this turn will produce is what the issue itself asks for, set `in_progress` FIRST (skip when the issue is already `in_progress`, or when your Agent Identity forbids status writes). Then complete the task within your Agent Identity boundaries.\n")
	b.WriteString("4. **Post your final results as a comment — this step is mandatory**: post it with `multica issue comment add` using `--content-file`. When the per-turn user message carries a triggering comment, reply in its thread with the `--parent` value it gives you for THIS turn. With no triggering comment, post a new top-level comment.\n")
	b.WriteString("5. Before exiting, confirm the status still matches where things actually stand.\n\n")
	b.WriteString("Status reflects the state the ISSUE is in: delivered work awaiting acceptance → `in_review`; work continuing beyond this turn → `in_progress`; missing something you need → `blocked` with a comment explaining the blocker. Questions, discussion and acknowledgements never touch status. `done` stays human.\n\n")
	b.WriteString("## Important: Always Use the `multica` CLI\n\n")
	b.WriteString("Access Multica platform resources only through the `multica` CLI — never `curl` / `wget`. For anything the CLI doesn't cover, post a comment mentioning the workspace owner rather than working around it.\n\n")
	b.WriteString("## Output\n\n")
	b.WriteString("⚠️ **Final results MUST be delivered via `multica issue comment add`.** The user does NOT see your terminal output or run logs — only comments on the issue.\n\n")
	b.WriteString("**Post exactly ONE comment per run — your final result, before this turn exits.** Do NOT post progress updates or plans along the way.\n\n")
	b.WriteString("Keep comments concise and natural — state the outcome, not the process.\n\n")
	b.WriteString("**Runtime-local paths are never deliverables.** Your working directory exists only in this disposable sandbox — NEVER write an absolute path or a `file://` URL as a clickable link or an embedded image. Reference code locations as inline code.\n")
	return b.String()
}
