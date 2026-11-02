# ADR 0031: Controller self-deploy with a drain contract

Status: Proposed.
Date: 2026-11-02

## Context

Agents run inside this sandbox, so they cannot roll it out through the host's
Ansible runtime, which requires an idle queue and restarts the whole runtime.
`images.yml` publishes digests but leaves no machine-readable release, and the
controller has no way to stop claiming before a restart or to report readiness.

A restart interrupts attempts without losing them silently. SIGTERM cancels
running attempts without a terminal report and removes their containers (ADR 0007).
The next start calls `recover-orphans`; Multica v0.7.2 fails `dispatched` and
`running` tasks with `failure_reason=runtime_recovery`, then `HandleFailedTasks`
creates one retry while `attempt < max_attempts` (default 2), except for autopilot
and triage runs. The retry keeps `session_id` and `work_dir`, so a retained issue
session resumes (ADR 0015). Otherwise the task fails with a comment and its issue
returns to `todo`. A second interruption or an autopilot run is therefore lost
work, which a drain avoids.

## Decision

**Drain contract 1.** The controller image carries
`io.multica-sandbox.deploy-contract=1`. SIGUSR1 holds new claims, including
wakeup-triggered rounds; heartbeats, status polling, reports and running attempts
continue. SIGUSR2 releases the hold; a restarted process always starts undrained.
`sandbox_claims_draining` exposes the state. `GET /readyz` on the metrics listener
answers 200 once at least one workspace is registered and recovered, else 503;
`sandbox_workspaces_ready` exposes the count. No mutating endpoint is added: the
metrics listener is reachable from the execution network, signals are not.

**Release artifact.** `images.yml` stays manual. Each image also gets OCI revision
and source labels and a `release-<image>-<os>-<arch>` artifact with commit, ref,
image, digest and platform.

**Deploy workflow.** `deploy.yml` is manual, one run at a time, and runs only from
master in the `vps-worker` environment. It takes a master commit (default: head),
requires that commit to be on master, picks the newest successful `images.yml` run
with a linux/amd64 controller artifact, validates it and sends only
`sha256:<digest> drain=<minutes> [force]` to the host capability over SSH with a
pinned host key. The summary shows previous, requested and running digests,
drain outcome, readiness and rollback. Helper, CLI and agent pins are not part of
this path; they remain host infrastructure configuration.

**Host side** is owned by vps-worker ADR-0024: forced-command account, pull and
image checks, drain with a bound, live pin, controller-only recreation, readiness
and stability gate, automatic rollback.

## Consequences

An agent dispatches the deploy for a merged commit, sets a native Multica wakeup
and ends its run; the drain waits for that run. After the restart the wakeup
starts a new attempt on the new controller, which checks the run summary and its
own behavior. A drain timeout keeps the old controller and resumes claims unless
`force` is set; `force` accepts the recovery semantics above. Environment secrets
and the master-only environment rule are owner configuration. Readiness proves
registration and recovery, not end-to-end task execution.
