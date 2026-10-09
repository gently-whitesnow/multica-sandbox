# ADR 0017: Web chat tasks

Status: Accepted
Date: 2026-10-09

## Context

Multica chat sessions dispatch agent tasks without an issue. Before this decision,
the OpenCode adapter treated a chat claim like any other non-issue task. It used
the bounded legacy prompt, offered no repository checkout and always started a
fresh session.

Upstream Multica (`b4ca5b4`) handles chat claims as follows:
- The claim carries `chat_session_id`, the batch of user messages as
  `chat_message`, `chat_message_attachments`, and, for IM channels,
  `chat_channel_type`, `chat_type`, `chat_in_thread` and
  `chat_channel_delivers_files`. Historical intro sessions carry `chat_intro`.
- `chat_session` stores the resume pointer: `session_id`, `work_dir` and
  `runtime_id`. On a claim the server returns `prior_session_id` only for the same
  runtime, and `prior_work_dir`. Without a pointer it falls back to the latest
  chat task with a session. A withheld session sets
  `prior_session_resume_unavailable`. Completion and failure reports update the
  pointer; `retired_session_id` clears it.
- The daemon keys managed workdirs by (workspace, agent, chat session), like
  issue workdirs. A prior session resumes only in the same existing workdir.
  Otherwise the run starts fresh with a continuity notice chosen by surface:
  issue, Slack channel history, stored transcript (web chat, Feishu, WeCom,
  DingTalk) or unrecoverable.
- `BuildPrompt` renders `buildChatPrompt`: audience, channel guidance, the user
  message, attachment IDs and file delivery. The runtime brief has the same
  header, identity, commands, repositories and project sections as issue tasks.
  It omits comment formatting and uses the chat workflow and chat output
  sections.
- Repository access is unambiguous upstream. `resolveClaimProjectContext` gives
  chat claims the chat project's `github_repo` resources, or the workspace
  repositories. The chat workflow says to use `multica repo checkout` for code
  changes, and the daemon's checkout endpoint serves chat tasks like issue tasks.
- `multica chat history` reads the stored transcript through
  `/api/chat/history` with the task token.

## Decision

Follow upstream for web chat claims when the Multica relay is configured.
Channel-backed chats (`chat_channel_type` set: Slack, Feishu and others) keep
the legacy prompt and a per-attempt workdir until their delivery is verified
end to end.

**Claims.** The claim parser reads the web chat fields and the channel type.
A chat session ID that is not a UUID rejects the claim. So do more than 64
attachments, attachment IDs that are not UUIDs, and oversized or multi-line
attachment metadata. A `chat_type` the server
does not send reads as an unknown audience, never as a direct room. Chat wins
over issue fields, as in upstream.

**Prompt and brief.** Web chat tasks get upstream's chat prompt and stored
transcript continuity notice byte for byte, and the issue brief's sections with
upstream's chat workflow and web chat output. The skill selection block is omitted because the adapter
does not project skills. Without the relay the agent has no CLI, so chat keeps
the legacy prompt.

**Workdirs and resume.** `execution.Workdir` gains a chat key. Session volumes
are named and labelled from (controller, workspace, agent, `chat:<session>`).
An issue whose ID equals a chat session ID therefore gets a different volume.
ADR 0015's rules apply unchanged: one writer with a 15 s wait, resume only when
`prior_work_dir` names that pre-existing labelled volume, a fresh retry that
retires a failed resume, the idle TTL and the count cap.

**Repositories.** Chat tasks get the claim's repositories through the same
checkout endpoint, Git relay and forge relay grants as issue tasks.

Rejected alternatives:
- Keeping chat on the legacy prompt with repository references only. It
  diverges from the native runtime and tells the agent nothing about the
  CLI, the stored transcript or file delivery.
- Keying chat volumes by issue-style labels. A chat and an issue could then
  share state.
- Rendering channel surfaces from upstream text alone. Their history readers
  and file delivery depend on server adapters this fixture does not exercise.
- Rejecting channel claims. It would fail channel chats that get the legacy
  prompt today.

## Consequences

Web chat follow-ups continue the native OpenCode session in the chat's
retained workdir. The integration fixture verifies this together with a
transcript read through the relay. Channel chats stay on the legacy prompt
without resume. Chat attachments (`multica attachment upload`/`download`) and
intro sessions are verified only as rendered text. Their server-side delivery is not exercised. Chat volumes
count toward the same session cap and TTL as issue volumes. Upstream's
chat-session garbage collection (`GetChatSessionGCCheck`) is not mirrored;
idle TTL and the cap bound retention, as for issues before #60.
