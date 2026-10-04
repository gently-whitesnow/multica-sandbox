# ADR 0001: Independent runtime with isolated task execution

Status: Accepted
Date: 2026-10-04

## Context

Multica intentionally delegates execution isolation to its operator. We need
disposable task environments, controlled access and reusable preparation caches
without maintaining a Multica fork. Custom runtime profiles replace CLI launch
but leave workspace preparation with the external daemon.

## Decision

Implement an independent controller and single-task runner. Register logical
Codex/Claude runtimes through Multica's Daemon API without runtime profiles.
Multica remains responsible for tasks and retries; the controller handles claims,
leases, execution events, cancellation and reconciliation after restart.

Start with Docker/Sysbox, then add a Kubernetes backend. Isolate preparation and
execution; constrain mounts, egress, credentials, resources and lifetime. Keep
controller credentials and host sockets outside agent environments. A runner may
report only its own execution through its controller channel.

Pin toolchains by digest. Key immutable preparation caches by repository revision,
lockfiles, toolchain, platform and setup recipe. Each run gets private writable
state and fresh authorization; credentials never enter cached snapshots. Preserve
workspace/session state explicitly for resume, with exclusive writers and retention.

Track current Multica releases through compatibility tests and coordinated
upgrades. Daemon API is an integration dependency, not a guaranteed stable SDK.
Do not assume the upstream CLI provides the fork's single-task runner.

## Consequences

We own runner compatibility, context delivery, skills/MCP, resume, cancellation
and cleanup. The first milestone must prove this lifecycle against upstream
before claiming production readiness. Containers share a host kernel; retained
state and granted credentials remain part of the trust boundary.

## References

- [Multica security model](https://multica.ai/docs/security-model)
- [Upstream Daemon API client](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/daemon/client.go)
- [chrissnell controller: architectural reference](https://github.com/chrissnell/multica/tree/3829cb503922d940d332fd1c7c64ec348e80159c/server/cmd/multica-k8s-controller)
