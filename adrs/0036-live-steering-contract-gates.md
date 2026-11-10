# ADR 0036: Native live steering through an owned OpenCode service

Status: Accepted, opt-in native HTTP path
Date: 2026-11-10

## Context

Steering adds guidance before the original run finishes; resume/coalescing remain
separate. Multica v0.7.2 is pinned at `f05a525ac8e82f5e9169ad078e44f166fad6d5c8`.
Its supplements API exists, but excludes `opencode` at start negotiation, direct
creation and composer binding. Inspected main `fa089093fd91bb039dcb9b22c680e58b279abcfe`
retains these gates. `examples/live-steering/native-admission.patch` changes six
SQL/generated-query lines, without migration. It runs only in disposable clones;
corporate admission requires a separate reviewed server change. Native Multica's
own daemon additionally needs its provider hook.

## Decision

Keep ACP default. `opencode.live_steering` selects one owned OpenCode 2.2.5 HTTP
service/session per attempt, on container loopback with an ephemeral password.
`wget`, `sync` and `timeout` are additional image gates. Custom commands must provide
native serve readiness. Existing MCP grants, model/effort, permissions, skills,
workdirs and reporting remain configured. Offer `task-supplement-v1` only for this
adapter's eligible issue claims and require its `/start` echo. Unmodified servers
refuse negotiation, keep native queued/deferred handling and return 412 for direct
supplements. Steering never substitutes a run or cancels/restarts execution.

### Alternatives compared

| Path | Evidence and choice |
| --- | --- |
| Concurrent ACP prompt | Pinned 1.18.34/1.18.35 return generic RPC deadlines; 2.2.2/2.2.5 reject the active prompt. No usable processing receipt. |
| Native HTTP | Positive same-service/session mid-turn probes on 2.2.5/2.2.7. Inbox inspection/cancellation supports cleanup; only 2.2.5 is admitted. |
| Server plugin SDK | Positive historical 2.2.5 comparison inside the original ACP owner, but no public inbox cancellation. Superseded prototype removed; evidence remains in Git history. |
| Legacy `prompt_async` | Inspected 1.18.35: 204 forks processing; persistence precedes `ensureRunning`. Neither processing receipt nor conditional active-turn admission; not admitted. |

ACP's private service cannot be addressed through an unrelated HTTP service. HTTP
mode owns its service from initial input onward.

### Delivery and recovery

The controller journal binds server, workspace, runtime, task, exact dispatch and
session. Atomic replacement plus file/directory fsync persist intent before claim,
content/stable ID before admission, consumption before ack. Records contain private
guidance (0600), no controller/claim credentials. Recovery assumes one claimant for
the run, under the existing single-controller identity rule.

An ambiguous claim is never repeated blindly. Receipt IDs and complete timeline
inspection use the in-memory task token. Missing/ambiguous/truncated data fails
closed. Inspection is bounded to 4096 IDs and 2 MiB; large timelines can prevent
delivery and need an upstream bounded receipt lookup. Empty/already-acked ID sets
skip timeline and `/start` inspection. Public timestamps lose microseconds; native
idempotent `/start` replay fences the exact dispatch before claim/delivery.

Native `daemon:task_supplement_available` hints wake only the matching negotiated
runtime/task (ADR 0021). Hints coalesce; 30-second polling remains the fallback.
Claims drain serially. Stable input IDs deduplicate admission/history;
`delivery: steer, resume: false` schedules no substitute execution. Acknowledgement
requires canonical input, inbox promotion and a subsequent step in that execution.
Check in-memory proof before canonical reads; cache successful validation.
Definite claim 4xx except 408 reset intent; uncertain outcomes retain it.

Ack/status failures use persisted exponential backoff (4 seconds–5 minutes).
Missing tasks retire active records. Terminal permanent ack refusals quarantine
consumption evidence outside replay, without claiming native delivery. Running or
unknown task/receipt refusals retain evidence and pause serial delivery: a 4xx alone
cannot safely discard it. Quarantine is controller-private and needs operator
investigation/collection. Restart replay respects backoff and precedes terminal
report/recovery processing; it never reinjects old guidance.

Join the worker before completion and withdraw pending inputs while the owner is
alive. DELETE 204 is ambiguous: only native inbox-cancelled proof preserves clean
continuity; unproved withdrawal remains unsafe. Real task cancellation interrupts
execution. Finite API requests close pipes on cancellation and bound child lifetime;
SSE disables wget read timeout; attempt watchdogs still bound missing progress. Native
2.2.5 `serve --stdio` exits on EOF, verified in a disposable owner.

Persist controller ownership before HTTP boot. Unknown/unclean state is quarantined
before native orphan recovery; retire its session with the lost-session notice.
Clean reuse requires empty active/inbox/shell state and no background/unknown work.
Disabling steering also retires HTTP state before ACP boot; ordinary ACP resumes
then continue normally. Returning to HTTP treats ACP state as unknown. At most one
quarantined DB/WAL/SHM set remains per volume; next preparation removes older sets.
It consumes the retained byte budget; workdir files/native comments remain.

## Consequences

Native claims have neither leases nor idempotent request tokens. Uncommitted
ambiguous requests can stall until terminal settlement; absence during inspection
cannot prove they will not commit later. No exactly-once or unconditional eventual
delivery promise is made. Eliminating these stalls needs upstream claim/inspection
extensions. Unsafe owners explicitly lose native session continuity. SSE disconnects
fail closed.

The loopback password is readable by agent processes through ENV/exec arguments.
The agent can access its own service, alter its DB or forge predictable IDs;
processing receipts assume a cooperative runtime, not attestation that an adversarial
agent/model saw or obeyed the text. Controller credentials remain outside. POST
bodies live in ephemeral `/tmp`; SIGKILL leftovers disappear with attempt cleanup.

Real fixtures cover mid-turn delivery, order, cancellation, controller SIGKILL,
HTTP→ACP isolation, native withdrawal/quiescence, deadlines/EOF/SSE and MCP rotation.
Stable-ID retry dedup and consumed-ack replay after crash currently use fake fixtures;
the real SIGKILL test covers unconsumed-claim recovery. Ordinary `./verify.sh` omits
real paths: run the patched SERVICE/OPENCODE and HTTP_RUNTIME fixtures documented in
`examples/live-steering/README.md`. A positive 2.2.7 probe does not admit that image.

## References

- [Native SQL](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/pkg/db/queries/supplement.sql)
- [Native reference loop](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/internal/daemon/task_supplement.go)
- [OpenCode 2.2.5 API](https://github.com/anomalyco/opencode/blob/9bbb8ce18c80f099e0e448775f59d965ead40a21/packages/protocol/src/groups/session.ts)
