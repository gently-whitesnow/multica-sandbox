# ADR 0021: Daemon WebSocket wakeups

Status: Accepted
Date: 2026-10-14

## Context

The controller answered pending runtime requests, such as a model list for the
Multica picker, only on its 15-second heartbeat (ADR 0008), so the picker waited up
to 15 seconds. The upstream daemon holds `/api/daemon/ws`: when the server queues
such a request it pushes `daemon:pending_work`, and the daemon heartbeats that
runtime at once.

## Decision

Hold the same WebSocket for the ready runtimes (`runtime_ids`, controller token)
and treat its frames only as hints, as upstream does:

- `daemon:pending_work` heartbeats that runtime now;
- `daemon:task_available` starts a claim round;
- `daemon:workspaces_changed` runs a workspace sync.

The connection is rebuilt when the runtime set changes and after failures with a
1–30 s backoff. A connection without frames or server pings for 60 s is dropped,
matching the upstream hub. Connection changes are logged once per state.

## Consequences

Model lists answer in about a second. Scheduled heartbeats, claim polling and
discovery stay the correctness path: lost, repeated or absent hints only cost
latency, and a server or proxy without the WebSocket leaves the controller polling.
Other upstream frames (heartbeat acks, RPC) are ignored. This adds
`github.com/coder/websocket`.

## References

- [Upstream daemon wakeups](https://github.com/multica-ai/multica/blob/d45aba1cd7582bef9210b921bbb7dc198b48e1ee/server/internal/daemon/wakeup.go)
- [Pending work hint](https://github.com/multica-ai/multica/blob/d45aba1cd7582bef9210b921bbb7dc198b48e1ee/server/pkg/protocol/messages.go)
