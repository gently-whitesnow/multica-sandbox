# ADR 0006: Offline Docker execution before external access

Status: Accepted
Date: 2026-10-04

## Context

The lifecycle probe establishes Multica integration but provides no execution
boundary. Containers outlive a crashed controller, so server orphan recovery must
not create another attempt while old execution survives. Issue #8 introduces an
offline test workload; identity, MCP and real agent adapters remain separate work.

## Decision

Use the maintained Docker CLI for create, inspect, start, inspect-state and forced
removal. Keep a small execution interface (reconcile, start, wait, remove) separate
from Multica. Do not implement an orchestrator or import Multica internals.

Evaluated on 2026-10-04: Docker Engine 29.2.1 with runc 1.3.4, Linux cgroup v2,
Docker Desktop 4.62.0 on arm64. Moby and Docker CLI are Apache-2.0; Desktop has
separate licensing. Docker supplies maintained lifecycle and kernel controls, but
its defaults need explicit hardening. The trusted controller requires daemon
access; the workload receives none. Operating costs include a trusted Docker host,
preloaded images and controller supervision. No cloud service is required.

Sysbox v0.7.1 (released 2026-07-31, Apache-2.0) is a maintained candidate with user
namespace isolation and support for system workloads. It requires additional
host components and is not installed in the test environment. This implementation
explicitly selects runc; Sysbox is neither silently selected nor advertised as
supported until the same conformance suite passes. Kubernetes remains deferred.

Require Linux, cgroup v2, CPU/memory/PID controllers and builtin seccomp. Refuse
unsupported hosts. Operators preload an OCI image pinned by digest and select an
absolute executable with arguments. No project-image inheritance is required.
Reject image-declared volumes; disable inherited healthchecks and entrypoints.
Image authors must not bake service credentials into their artifacts.

Each attempt receives a non-root UID, zero capabilities, no-new-privileges, private
namespaces, no network, a read-only root, 64 MiB workspace and 16 MiB temporary
tmpfs. Limit execution to 0.5 CPU, 128 MiB memory, no swap and 64 processes.
Limits are fixed for this first backend. No host mounts, device passthrough,
Docker sockets, environment forwarding or output/log collection are exposed.
Inspect the created container before starting it; reject policy mismatches.

Refinement (#43, 2026-10-07). Docker adds `noexec` to `--tmpfs` unless `exec` is
given, so `/workspace` silently became non-executable and toolchains could not
run built programs (`go test`, native packages). The agent already executes
arbitrary code through interpreters; the boundary rests on identity,
capabilities, seccomp, no-new-privileges, limits and network policy. `/workspace`
is therefore explicitly `exec,nosuid,nodev`; `/tmp` stays `noexec`. The policy
check and a containers test pin both.

Use deterministic names from daemon identity, task and dispatch fence; label
containers by their trusted controller owner. Before registration or Multica
orphan recovery, remove all owned containers. Abort recovery on cleanup errors.
Hold the existing process lock throughout. This requires one controller with the
same daemon identity, Docker endpoint, backend configuration and lock path on
restart. Multiple hosts or alternate lock paths are unsupported.

On completion, cancellation or timeout, reap execution before the terminal API
callback. On unrecoverable control-plane errors or local shutdown, attempt cleanup
using an independent bounded context and exit. Never rerun or requeue tasks locally. Backend
start errors leave Multica recovery to the next invocation. Forced removal also
reaps descendants and ephemeral state; unrelated owners are untouched.

## Consequences

An arbitrary test command can run offline, but this is not a production agent
sandbox or completion of issue #2. Preparation recipes, artifact extraction,
identity/MCP, inference, tool mounts and cache reuse are not implemented.
Container image loading and registry credentials remain operator responsibilities.

The controller enforces the execution timeout while alive. After SIGKILL, a
container remains bounded by kernel resource controls but survives until the
controller restarts. Deployments must supervise it; there is no independent
wall-clock lease/reaper yet. Docker unavailability can prevent cleanup: fail
closed and require operator recovery rather than claiming successful teardown.

The shared kernel, Docker daemon, CLI and controller host are trusted. Tests prove
specific policy properties on the recorded environment, not resistance to kernel
exploits or suitability for hostile enterprise multi-tenancy. Re-run conformance
on each deployment/runtime upgrade before claiming support. CI remains paused.

## References

- [Docker security](https://docs.docker.com/engine/security/)
- [Docker resource constraints](https://docs.docker.com/engine/containers/resource_constraints/)
- [Docker run controls](https://docs.docker.com/reference/cli/docker/container/run/)
- [Docker CLI](https://github.com/docker/cli)
- [Sysbox](https://github.com/nestybox/sysbox)
- [Sysbox v0.7.1](https://github.com/nestybox/sysbox/releases/tag/v0.7.1)
