# ADR 0007: Docker-managed controller service

Status: Accepted
Date: 2026-10-04

## Context

A standalone probe processes one attempt and exits. Operators need one deployable
controller service that keeps accepting work, without adding a custom watchdog,
reaper or host process manager. The maintainer accepted restart-dependent cleanup
for the Docker stage. This narrows issue #10's original independent-lifetime scope;
it does not implement an independent execution deadline.

## Decision

Ship a controller OCI image and Compose deployment. Use the host Docker socket to
create sibling execution containers; do not run Docker-in-Docker. Keep one active
controller per daemon identity and Docker engine. Docker restart policy manages
the controller; workload containers retain restart=no and never serve two attempts.

The service connects/reconciles once, then processes attempts sequentially. After
a terminal callback and confirmed cleanup it claims the next task in the same
process. A control-plane or cleanup error exits rather than continuing in an
uncertain state. A normal termination signal stops active execution before exit;
Multica reconciles the interrupted task on the next service start.

Persist a shared lock and identity binding in a named volume. Configure a stable
daemon UUID explicitly; bind the volume to that UUID, workspace, Multica origin
and Docker Engine ID before recovery. Reject incompatible or corrupt state.
Container names and ephemeral container IDs are not controller identity. Do not
scale this Compose service or run the same identity with different state volumes.

On each start, acquire the lock, validate the binding, remove owned execution
containers and then invoke Multica recovery. Only then validate image readiness
and begin accepting work. Keep the service itself outside execution-owner labels.
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
one-attempt experiments. Both entry points still execute operator-configured test
commands and fixed task results, not agent instructions or a coding adapter.

## Consequences

Installation needs Docker, Compose, configuration, a preloaded workload image and
a token file. No second project daemon, systemd integration or custom scheduler is
introduced. Keep the state volume and daemon identity across image updates. To
change scope, clean old attempts first and use a distinct identity/state volume.
Never delete the state volume while its controller is active.

Acceptance tests must exercise the actual Compose service: sequential fresh
attempts without process restart, duplicate-lock refusal, automatic restart after
fatal process failure, orphan cleanup/recovery, graceful stop and container
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
