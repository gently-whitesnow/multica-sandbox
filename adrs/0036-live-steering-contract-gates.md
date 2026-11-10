# ADR 0036: Live steering contract gates

Status: Accepted gates; OpenCode live delivery blocked
Date: 2026-11-10

## Context

Live steering adds a user's message to the currently executing task, before its
original turn finishes. Resume and coalesced follow-ups are separate behavior.
Multica v0.7.2, pinned at `f05a525ac8e82f5e9169ad078e44f166fad6d5c8`,
has the native task-supplements API and composer steering refined by upstream PR
8760 and subsequent author-aware changes. The old local checkout is not the contract.

Two independent gates currently prevent delivery through this adapter:

- Native `supplement.sql` admits only `codex`, `claude`, `grok`, `cursor` and
  `kiro` at start negotiation, direct creation and comment binding. `opencode`
  cannot negotiate `task-supplement-v1`, even if a client requests it. Upstream
  main `fa089093fd91bb039dcb9b22c680e58b279abcfe` retains these gates.
- OpenCode 2.2.2/2.2.5 rejects a concurrent ACP `session/prompt` for the same
  session with an active-prompt service error. The real 1.18.34/1.18.35 probe
  returns a generic JSON-RPC deadline error. On all four, the original tool
  remains active, no additional sentinel reaches fixture inference before
  cancellation, and cancellation answers the original prompt. This does not
  prove that 1.x persisted no input. ACP has no negotiated steering method here.

## Decision

Keep the production adapter's start capabilities empty. Never register OpenCode
as another provider, replace the run, or cancel/restart a turn to simulate steering.
Do not claim live delivery, acknowledgement, or exactly-once behavior. Add
reproducible native protocol tests and an ACP probe while these gates are closed.
Fixture-only supported-provider rows exercise the server contract; they do not
add a sandbox adapter for that provider.

The native capability is explicitly returned by `/start`; absence is unsupported,
not implicit consent to poll. Ordinary comments with `steer_task_ids` retain
native queued/deferred follow-up handling when binding fails. Direct supplement creation
instead returns 412 without a comment; the caller must choose a normal comment.

### Delivery contract observed in the fixture

Creation is idempotent by task, author and `client_request_id`. Claims order pending
rows by creation time and comment ID, then move one to `delivering`. There is no
claim lease or redelivery token: losing the successful claim response strands
that row until terminal settlement, and another claim may return the next row.
Consequently serial polling alone cannot guarantee delivery or ordering after an
ambiguous transport failure.

A delivered ack replays idempotently. A negative ack records a stable reason;
repeating it returns 409. Explicit user retry requeues a failed row only while
its exact task is running. Completion, cancellation and recovery settle pending
and delivering rows as `failed/turn_ended`; recovery creates a distinct task, with
no inherited supplements. A late delivered ack can promote an already-claimed
`failed/turn_ended` row. The protocol trusts the runtime's delivery assertion:
it cannot prove model consumption. Runtime-owner and workspace binding checks
protect claim/ack; a task's receipt cannot be acknowledged on another task.
Bound human messages remain historical comments with a visible failed receipt;
they are not automatically replayed as successful follow-up guidance.

## Consequences

Upstream must admit a conformant OpenCode runtime at all three SQL gates; its
native daemon would additionally need a `SupportsTaskSupplement` gate change.
A sandbox-only change cannot unlock
the native user action on the deployed v0.7.2 server. No server modification,
provider impersonation or production configuration change is part of this work.

OpenCode 2.2.5's native REST `session.prompt` accepts a caller-specified message
ID, `delivery: steer` and `resume: false`. It durably admits input without
scheduling a fresh turn. This is a candidate supported primitive, not a verified
adapter: ACP owns a private standalone service with an ephemeral endpoint and
credential. `opencode api --standalone` starts a different service; the default
API command discovers or starts a managed one. Neither targets ACP's owner.
Using this primitive requires a supported way to share the same service, prove
consumption within the original turn, and resolve completion races without
leaving stale inbox guidance for session resume.

Before advertising capability, implement an attempt-fenced delivery journal,
durable acknowledgement replay before recovery, serial admission, and explicit
handling of ambiguous claims. A stronger upstream claim/reclaim or inspection
contract is needed to guarantee recovery of a lost claim response. Conformance
must cover real mid-turn model consumption, transcript/session and usage
continuity, watchdogs, cancellation/timeout, transport failures, restart and
workspace/attempt isolation. The current evidence establishes the blockers and
native failure semantics; it does not satisfy those delivery gates.

## References

- [Pinned native SQL](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/pkg/db/queries/supplement.sql)
- [Native reference loop](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/internal/daemon/task_supplement.go)
- [OpenCode 2.2.5 ACP turn](https://github.com/anomalyco/opencode/blob/9bbb8ce18c80f099e0e448775f59d965ead40a21/packages/cli/src/acp/turn.ts)
- [OpenCode native prompt API](https://github.com/anomalyco/opencode/blob/9bbb8ce18c80f099e0e448775f59d965ead40a21/packages/protocol/src/groups/session.ts)
- `integration/task_supplements_test.go`; `examples/acp-spike/steering.go`
