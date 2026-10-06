# ADR 0007: Docker-managed controller service

Status: Accepted
Date: 2026-10-04

## Context

A standalone probe processes one attempt and exits. Operators need one deployable
controller service that keeps accepting work, without adding a custom watchdog,
reaper or host process manager. Docker deployments use restart-dependent cleanup;
independent execution deadlines belong to the Kubernetes backend.

## Decision

Ship a controller OCI image and Compose deployment. Use the host Docker socket to
create sibling execution containers; do not run Docker-in-Docker. Keep one active
controller per daemon identity and Docker engine. Docker restart policy manages
the controller; workload containers retain restart=no and never serve two attempts.

The service discovers accessible workspaces and processes attempts through one
shared scheduler. Global concurrency limits capacity; one accessible workspace
uses the same execution path as many. A slot becomes available only after
confirmed cleanup and a terminal callback that Multica acknowledged or that is
durably queued in the state volume. Queued callbacks hold no credentials and are
replayed with backoff, before workspace recovery on start, so recovery does not
rerun finished work. Like the upstream daemon, transient Multica errors are retried
or deferred; protocol violations and cleanup uncertainty exit. A normal termination signal stops active execution before exit;
Multica reconciles the interrupted task on the next service start.

Persist a shared lock and identity binding in a named volume. Configure a stable
daemon UUID explicitly; bind the volume to that UUID, Multica origin and Docker
Engine ID before recovery. Reject incompatible or corrupt state, unknown identity
fields and trailing documents.
Container names and ephemeral container IDs are not controller identity. Do not
scale this Compose service or run the same identity with different state volumes.

On each start, acquire the lock, validate the binding, remove owned execution
containers, validate image readiness and invoke workspace-scoped Multica recovery
before accepting work. Keep the service itself outside execution-owner labels.
A missing execution image must not prevent removal of existing executions.

Use restart=unless-stopped and a graceful-stop window longer than the cleanup
request timeout. Manual stop intentionally leaves the service down. Restart policy
handles process exit, not a live hang, broken configuration or an unavailable
Docker host. It cannot promise a bounded cleanup time during those failures.
No healthcheck is presented as an automatic restart mechanism.

The controller runs as root inside its container to access the standard host
socket and persistent state. It has no privileged mode or host PID namespace,
drops capabilities and uses a read-only root. Socket access still grants Docker
administrative power: these settings do not isolate the controller from the host.
Prefer a dedicated worker host. Mount config read-only and the Multica token as a
controller-only secret file, never in workload environment or build context.
Compose file-backed secrets are host files, not an encrypted secret manager.

Pin the build and Docker CLI base images by digest. Build the controller locally;
registry publishing is separate work. Preserve the existing probe for explicit
one-attempt experiments. The default service and standalone probe execute offline test commands. An explicit
OpenCode configuration enables the experimental agent/identity path under ADR 0011;
optional inference identity follows ADR 0012. Full event/session conformance
remains separate work.

## Consequences

Installation needs Docker, Compose, configuration, a preloaded workload image and
a token file. No second project daemon, systemd integration or durable task queue
is introduced. Keep the state volume and daemon identity across image updates.
Ownership changes require execution cleanup and a distinct identity/state volume.
Never delete the state volume while its controller is active.

Acceptance tests must exercise the actual Compose service: fresh attempts through
the shared scheduler without process restart, duplicate-lock refusal, automatic
restart after fatal process failure, orphan cleanup/recovery, graceful stop and container
recreation with unchanged identity. Manual Docker kill/stop is not a substitute
for an internal process-crash test because it can suppress automatic restart.

Kubernetes HA and backend-enforced deadlines remain future work. They require
external attempt ownership, stale-owner rejection and recovery beyond local locks.
Preparation cache and progress checkpoints are separate from disposable execution.
Neither requires keeping a used container alive. CI remains disabled.

## References

- [Docker restart policies](https://docs.docker.com/engine/containers/start-containers-automatically/)
- [Docker daemon security](https://docs.docker.com/engine/security/)
- [Compose services](https://docs.docker.com/reference/compose-file/services/)
