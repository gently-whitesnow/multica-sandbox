# ADR 0036: Native live steering through an owned OpenCode service

Status: Accepted, opt-in native HTTP path
Date: 2026-11-10

## Context

Steering adds guidance before the original run finishes; resume/coalescing remain
separate. Multica v0.7.2 is pinned at `f05a525ac8e82f5e9169ad078e44f166fad6d5c8`.
Its supplements API excludes `opencode` at start negotiation, direct creation and
composer binding, and gives a daemon no bounded read of its receipts: only the
truncated issue timeline carries their status. Inspected main
`fa089093fd91bb039dcb9b22c680e58b279abcfe` retains both gaps. Three fixture patches
close them, without migration:

- `examples/live-steering/native-admission.patch`: six SQL/generated-query lines.
- `native-receipts.patch`: `GET /api/daemon/tasks/{id}/supplements[?comment_id=]`,
  authorized as claim/ack, returns the run status, the oldest unacknowledged claim
  with its claim payload and one receipt's status (`none` once deleted).

- `native-routing.patch`: persist member routing in `comment.suppressed_agent_ids`
  at create/bind, before claim. Claim no longer locks supplement then comment,
  opposite to deletion's order. A new bind cannot expose its uncommitted receipt
  to deletion; create inserts routing with the new comment.

They run only in disposable clones; deployment requires separate review.

## Decision

Keep ACP default. `opencode.live_steering` selects one owned OpenCode 2.2.5 HTTP
service/session per attempt, on container loopback with an ephemeral password.
`wget`, `sync` and `timeout` are additional image gates. Offer `task-supplement-v1` only for
eligible issue claims, after the server answers receipt inspection for the
dispatched task, and require its `/start` echo. Other servers keep native
queued/deferred handling and return 412 for direct supplements. Steering never
substitutes a run or cancels/restarts execution.

### Alternatives compared

| Path | Evidence and choice |
| --- | --- |
| Concurrent ACP prompt | ACP deadlines/rejections; no processing receipt. |
| Native HTTP | Same-session probes and inbox cancellation; only 2.2.5 admitted. |
| Server plugin SDK | No public inbox cancellation; prototype removed. |
| Timeline receipt inspection | Complete issue history cannot support bounded restart recovery. Replaced. |

### Delivery and recovery

The journal binds server/workspace/runtime/task/dispatch/session. Atomic
replacement and file/directory fsync persist intent before claim, stable input
before admission, consumption before ack. Records are private (0600), without
credentials. Recovery requires one claimant per run.

The native receipt is the authority; a claim response is a hint. Each round delivers the oldest unacknowledged claim, claiming only when none
exists and reading the result back. A lost claim response is adopted, never
claimed again; an uncommitted one is retried after backoff. Issue size and receipt
count do not matter. Idempotent `/start` fences the dispatch.

Native `daemon:task_supplement_available` hints wake only the matching negotiated
runtime/task (ADR 0021); 30-second polling remains the fallback. Claims drain
serially. Stable IDs deduplicate admission/history;
`delivery: steer, resume: false` schedules no substitute execution. Acknowledgement
requires canonical input, inbox promotion and a subsequent step in that execution.
Definite claim 4xx except 408 reset intent. An undeliverable payload is failed
with `provider_rejected` and draining continues.

Failures persist backoff (4 seconds–5 minutes); workers observe retry deadlines
and wake after replay settles consumption.
Missing tasks retire active records. A refused acknowledgement is resolved from the
exact receipt: `delivered` counts as acknowledged; `delivering` or a late
`turn_ended` receipt is retried; a 409, or any permanent refusal of a terminal run,
whose receipt left delivery quarantines the consumption evidence, never claims
native delivery, and a running attempt continues with the next input. Other
refusals and unreadable receipts retain the evidence and back off: they say
nothing about the receipt. Multica refuses comment deletion while a receipt is
pending/delivering, so one vanishes only through cascades or repair. A claim that
vanished before consumption is dropped. Restart replay respects backoff, precedes
terminal report/recovery processing and never reinjects guidance.

Quarantined evidence lives in `supplements/quarantine`, controller-private,
outside replay and unrelated to the OpenCode DB quarantine in session volumes. At
cold start and at most every 10 minutes, collection removes entries older than 30
days and the oldest beyond 512 entries or 64 MiB (an entry holds one comment of at
most 64 KiB), plus temporary files older than an hour. It reads names and metadata
only, never follows or removes symlinks, directories or foreign names, excludes
concurrent writes and runs synchronously. Active journals are never candidates.

Join the worker before completion and withdraw pending inputs while the owner is
alive. DELETE 204 is ambiguous: only native inbox-cancelled proof preserves clean
continuity. Finite API requests close pipes on cancellation and bound child
lifetime; attempt watchdogs still bound missing progress.

Persist controller ownership before HTTP boot. Unknown/unclean state is quarantined
before native orphan recovery, with the lost-session notice. Clean reuse requires
empty active/inbox/shell state and no background/unknown work.
Disabling steering retires HTTP state before ACP boot; returning treats ACP state
as unknown. At most one quarantined DB/WAL/SHM set remains per
volume, within the retained byte budget; workdir files/native comments remain.

## Consequences

Routing writes affect all admitted providers' member steering, even with the
sandbox flag off. Rollback leaves that per-agent suppression in place; content
edits after receipt removal recompute it from the edit request. If a receipt
vanishes before consumption, the member input can be silently lost: no delivery,
follow-up or visible receipt. Suppression preserves the explicit current-turn
choice; the server cannot reconstruct consumption.

Native claims have neither leases nor request tokens. Delivery follows native
claim order; a lost-response claim that commits after a later claim is delivered
after it. No exactly-once or eventual delivery is promised. An unclean restart loses
continuity; unconsumed claims settle `turn_ended`. SSE disconnects fail closed.

The loopback password is readable by agent processes. The agent can access its
own service, alter its DB or forge predictable IDs; processing receipts assume a
cooperative runtime, not attestation that an adversarial agent/model saw or obeyed
the text. Controller credentials remain outside. POST bodies live in ephemeral
`/tmp`; SIGKILL leftovers disappear with attempt cleanup.

Real fixtures (patched server, controller, OpenCode 2.2.5) cover order,
lost claim/ack responses, a resent admission deduplicated by its stable ID,
controller SIGKILL between proven consumption and acknowledgement, a vanished
receipt on a 5000-comment issue, cancellation, HTTP→ACP isolation and MCP rotation.
Ordinary `./verify.sh` omits them: `examples/live-steering/README.md` lists the
commands and the activation order.

## References

- [Native SQL](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/pkg/db/queries/supplement.sql)
- [Native reference loop](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/internal/daemon/task_supplement.go)
- [OpenCode 2.2.5 API](https://github.com/anomalyco/opencode/blob/9bbb8ce18c80f099e0e448775f59d965ead40a21/packages/protocol/src/groups/session.ts)
