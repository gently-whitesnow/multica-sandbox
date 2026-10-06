# ADR 0005: Prove the daemon contract with a credential-blind probe

Status: Accepted
Date: 2026-10-04

## Context

Upstream has no stable external daemon SDK. Its client is an internal Go package;
importing it would couple this runtime to upstream implementation details. Before
adding isolation backends, we need evidence that the supported HTTP lifecycle
works without profiles or a fork.

## Decision

Use a small Go standard-library HTTP client and a single-task lifecycle probe.
Register provider `sandbox-probe` under an explicit, stable daemon UUID. The name
marks a test executor, not a supported coding agent. No profile or unsupported
capability is advertised. Use only an isolated test workspace: completing a probe
task is not evidence that its requested work was performed.

Test upstream revision `b4ca5b4a23e68b26292a680dca7689a952bb1cd5` (main inspected
2026-10-04). Build its unmodified server and migrations in a disposable fixture.
Seed users, workspace and queued tasks in fixture SQL; exercise registration,
claims, preparation lease, start, heartbeat, messages, completion, failure,
cancellation and orphan recovery over HTTP. This does not test UI task creation.

Require the claim's runtime ID, dispatch timestamp and start-claim support.
Never start execution before acknowledgment. Decode only the small lifecycle
projection; drop credentials and other claim context. Keep authentication inside
the trusted client. Reject redirects and non-TLS origins except literal loopback
addresses. Withhold server error bodies from logs.

The fake executor only waits and emits fixed text. It executes no process, prompt,
workspace preparation or tool call. Run one process per daemon identity, protected
by an OS lock at a shared trusted path. Register and recover orphans before
claiming after restart. Multica owns retries; the probe never requeues work.
Never replay claims or execution. As in the upstream daemon, transient transport,
5xx, 408 and 429 errors are retried for the dispatch-fenced start and terminal
callbacks and tolerated for transcript, heartbeat and status calls; other
control-plane errors stop the probe. A subsequent invocation reconciles with the server.

## Consequences

This is an integration experiment, not a production runtime or isolation boundary.
Credentials remain in the trusted probe process, which must never run agent code.
No Docker/Sysbox execution backend, MCP gateway, model adapter or session resume
is implemented. The fixture's Docker containers host test infrastructure only.

Recovery currently assumes the old executor died with the controller. A real
backend must stop/reconcile surviving executions before invoking recover-orphans.
The local lock is not distributed fencing: operators must never run the same
identity on different hosts or lock paths. HA needs a separate decision.

Terminal callbacks have no attempt fence in this client contract. Do not infer
exactly-once execution, atomic cancellation/completion, or safe arbitrary retries.
No durable event outbox or automatic runtime re-registration is provided. Extend
capabilities only after the corresponding contract and failure tests exist.

## References

- [Delivery task](https://github.com/gently-whitesnow/multica-sandbox/issues/1)
- [Upstream client](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/daemon/client.go)
- [Upstream recovery handler](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/task_lifecycle.go)
