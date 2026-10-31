# ADR 0026: Multica v0.7.2 and bounded task capabilities

Status: Accepted
Date: 2026-10-31

## Context

The controller was pinned to Multica v0.5.2. v0.7.2 is the stable release at this
upgrade, commit `f05a525ac8e82f5e9169ad078e44f166fad6d5c8`. The controller may need
workspace admin access for trusted configuration and workspace environment reads.
That must not give the executing agent administrative API access.

At this revision, claim delivery locks the runtime and sets task_token.user_id to
its owner. Auth resolves that user and stamps the task's agent and workspace.
Workspace-role middleware still sees the runtime owner's role. A real admin-owned
mat_ token can update a workspace; agent attribution alone does not downgrade it.
A new denylist would leave future or overlooked administrative routes exposed.

Native Telegram chats and run-only autopilots are required task shapes. The old
projection treated channels as legacy prompts and dropped autopilot instructions.

## Decision

Pin the disposable upstream fixture and mounted CLI artifact to v0.7.2. Verify the
release's HTTP contracts against the unmodified server; do not patch Multica.
Keep the administrative controller PAT and upstream task token controller-side.

Add a method-and-route allowlist to the Multica task relay. Only these member
workflows are available:

- Issue list/search/get/create/update, comments and bounded task/timeline reads.
- Issue wakeups: list/create/update/delete, enable/disable/trigger/check-in/runs.
- Read-only project/resources, workspace/member and redacted agent descriptions.
- Chat transcript/history/thread reads where the native server supports them.

Everything else is refused before forwarding: workspace and membership mutations,
agent configuration and credentials, global automation configuration, system
wakeups, runtime management, token issuance and new upstream routes. The trusted
controller calls its own API client directly. Repository checkout is a separate
controller route, authorized by the existing attempt and repository grants.

This is a capability restriction at the relay, not a claim that the upstream
mat_ token became a member token. Network policy must keep Multica itself off
attempt networks. Existing server checks still bind the task to its workspace,
agent actor and concrete resources. Issue operations may trigger other agents
within upstream authorization; this policy does not create a second policy engine.

Preserve native run-only autopilot instructions, identifiers and bounded trigger
payloads. Their final assistant output is the run result, with no invented issue
comment requirement. Autopilot configuration stays outside task capabilities.

Use native chat prompt, channel audience and delivery context for channel chats,
and the same retained conversation volume/session contract as web chats. Follow
v0.7.2's channel history and lost-session rules: it does not advertise Telegram
history recovery. A retained session continues; if it is lost, report that honestly.
Run-scoped project workflow and originator context stay out of the stable brief.

## Alternatives

- Reusing an admin task token behind the old denylist: rejected; newly introduced
  administrative surfaces would be admitted by default.
- Rewriting the user/role headers: rejected; the server overwrites them from mat_.
- Sending a separate member PAT: rejected; it loses the authenticated agent actor
  and task binding. Separate member-owned runtimes remain an alternative when the
  deployment needs upstream member identity instead of bounded capabilities.
- Downgrading Multica or forking its authorization: unnecessary for this boundary.

## Consequences

Some native CLI commands intentionally return a relay refusal; a compatible server
or CLI version does not grant new capabilities automatically. Admit additions only
after inspecting the pinned router/handler and adding a negative contract test.
The task relay is not a complete native platform API and exposes no admin routes.

The upstream fixture demonstrates that an admin task token directly updates the
workspace, while the relay refuses that same operation and credential/management
routes. Issue reads and agent-authored comments work; ended grants fail closed.
Synthetic Telegram snapshots exercise claims and follow-up session continuity
with credential-free synthetic routing, no valid recipient and no outbound messages. Run-only
claims exercise instructions and terminal run results. OpenCode unit tests cover
channel audience, lost sessions and attempt workdir isolation.

## References

- [Stable release](https://github.com/multica-ai/multica/releases/tag/v0.7.2)
- [Claim delivery authorization](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/internal/handler/daemon.go)
- [Pinned routes](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/cmd/server/router.go)
- `integration/task_authority_test.go`, `integration/native_claims_test.go`
