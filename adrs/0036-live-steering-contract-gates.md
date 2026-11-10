# ADR 0036: Native live steering through an owned OpenCode service

Status: Accepted, opt-in native HTTP path
Date: 2026-11-10

## Context

Live steering adds guidance before the original Multica run finishes. Resume and
coalesced follow-ups remain separate. Multica v0.7.2 is pinned at
`f05a525ac8e82f5e9169ad078e44f166fad6d5c8`; its native supplements API is real.
The old local checkout is not the contract.

Unmodified v0.7.2 excludes `opencode` at three SQL gates: start negotiation,
direct supplement creation and composer binding. The inspected upstream main
`fa089093fd91bb039dcb9b22c680e58b279abcfe` retains these restrictions.
`examples/live-steering/native-admission.patch` adds OpenCode at those gates and
their generated queries: six changed lines, no schema migration. The fixture
applies it only to a disposable pinned clone. Corporate admission needs a
separate reviewed server change; no installed server is changed here. Native
Multica's own daemon would additionally need its provider supplement hook.

## Decision

Keep ACP as the default. `opencode.live_steering` opts into one explicitly owned
OpenCode 2.2.5 HTTP service per attempt, authenticated with an ephemeral password
and bound only to container loopback. The maintained native API and SSE stream
address that same service/session. `wget` and `sync` are additional image gates;
custom commands in this mode must provide the native serve readiness protocol.
Selected MCPs use the existing relay and opaque grants; provider/model/effort,
permissions, skills, repository workdirs and reporting retain their configuration.

Offer `task-supplement-v1` only for this configured adapter and eligible issue
claims. Require the capability echoed by `/start`. An unmodified server refuses
negotiation and keeps ordinary queued/deferred comment handling; direct supplement
creation returns 412. Never impersonate another provider or replace/cancel/restart
an execution to steer it.

### Maintained alternatives compared

| Path | Evidence and choice |
| --- | --- |
| Concurrent ACP prompt | Real pinned 1.18.34/1.18.35 return a generic RPC deadline; 2.2.2/2.2.5 reject the active prompt. No usable steering acknowledgement. |
| Native HTTP service | Positive same-service/session mid-turn probes on 2.2.5 and 2.2.7; selected for its inbox inspection/cancellation and stable message IDs. Only 2.2.5 has adapter admission. |
| Server plugin SDK | Positive 2.2.5 probe calls `ctx.session.prompt` inside the original ACP owner. Its public session domain lacks inbox cancellation needed for late-input cleanup; kept as a comparison probe. |
| Legacy `prompt_async` | Inspected 1.18.35: 204 forks prompt processing; persistence precedes `ensureRunning`. This is not a processing receipt or a conditional active-turn admission. Not admitted here. |

ACP's private service cannot be targeted with an unrelated API service. HTTP mode
owns its service from initial input onward; it does not switch owners mid-turn.

### Delivery and recovery

The controller journal binds server, workspace, runtime, task, exact dispatch and
session. Atomic replacement, file and directory fsync persist intent before claim,
claimed content/stable input ID before admission, and consumption before ack.
Controller/claim credentials stay out of records; guidance is private controller
state, mode 0600. Recovery assumes this controller is the run's sole claimant,
under the existing single-controller identity rule.

Claims are never retried blindly. After an ambiguous reply, inspect the exact
task's receipt IDs and full timeline using its in-memory task token. Require all
receipts and reject truncation, missing metadata or ambiguous candidates. Public
run timestamps lose microseconds, so an idempotent `/start` replay verifies the
original runtime/dispatch fence before claiming and delivering.

Drain serially. Native input IDs deduplicate admission and canonical history.
`delivery: steer, resume: false` admits without scheduling a new execution.
Acknowledgement requires canonical user input, its native inbox-delivered event
and a subsequent step in the same execution. This is a native processing boundary,
not proof that a model obeyed the guidance. The deterministic fixture separately
checks that the model sees it before original completion. Human/agent authorship
retains native authority distinctions.

Join the worker before completion; reconcile ambiguous admission while the owner
is alive. Withdraw unconsumed pending inbox entries on completion/cancel races.
Never interrupt execution for steering. Real task cancellation still interrupts
it. Replay durable success acknowledgements before terminal reports and orphan
recovery; restart never reinjects an old attempt's journal.

An authoritative controller-owned owner receipt is persisted before server boot.
Unclean, unknown or uncertain retained state is quarantined before OpenCode can
auto-resume orphaned executions under new credentials. Retire that session and
report the existing lost-session notice. Clean resume requires native active,
inbox and shell checks and no observed background/unknown work; uncertainty keeps
the owner unclean. The agent's marker cannot authorize reuse. These observations
are not attestation of an agent-controlled database.

## Consequences

Native claims have no lease or idempotent request token. An ambiguous request that
never committed can stall delivery until terminal settlement; absence during
inspection does not prove it cannot commit later. A truncated timeline also stalls.
Native comments and failed receipts remain visible; no silent substitute run is
created. A delivered ack replays, including after settlement of a claimed row.
There is no exactly-once or unconditional eventual-delivery promise across crashes.
Progress through those ambiguities needs an upstream idempotent claim/inspection
extension. Interrupted or uncertain HTTP owners lose native session continuity;
workdir files and native comments remain, and the loss is explicit.

Conformance uses disposable fixtures: admission/fallback, mid-turn consumption,
order/dedup, HTTP response loss, journal phases, cancellation/completion, restart
state and isolation. SSE is volatile: disconnection fails closed. OpenCode 2.2.7's
positive transport probe does not admit that release or other adapters.

## References

- [Native SQL](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/pkg/db/queries/supplement.sql)
- [Native reference loop](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/internal/daemon/task_supplement.go)
- [OpenCode 2.2.5 API](https://github.com/anomalyco/opencode/blob/9bbb8ce18c80f099e0e448775f59d965ead40a21/packages/protocol/src/groups/session.ts)
- [OpenCode 2.2.7 ACP owner](https://github.com/anomalyco/opencode/blob/fedc6f208aa2bd54c1019227214568083f6380ad/packages/cli/src/commands/handlers/acp.ts)
