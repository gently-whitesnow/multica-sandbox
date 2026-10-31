# ADR 0028: Host-sized pools and paced admission

Status: Accepted. Date: 2026-10-31.

## Context

A fixed allocation per logical agent wastes idle capacity and cannot follow a host
resize. Memory PSI alone cannot stop a cold burst: the claim loop can fill all
slots before new compilers or nested engines establish their working sets. Retained
session counts do not bound bytes on the Docker filesystem.

## Decision

The deployment sizes the bounded attempt slice from host capacity, reserving at
least 2 GiB or 20% of RAM for the kernel, engines, controller, gateway and proxy.
MemoryHigh is 85% of the resulting MemoryMax. Swap is bounded by existing host
swap; provisioning does not create an unlimited swap file. The controller remains
outside the attempt pool. Logical agent roles own no memory reservation.

The explicit `resources.cpu_mode: shared` removes per-attempt CPU quotas; omitting
it preserves the fixed legacy default. `resources.cpus` conflicts with shared mode.
The deployment derives a global concurrency ceiling from host CPUs, capped by the
controller's supported maximum. This ceiling is not a promise of parallel builds.

`pool.auto_admission: true` requires `pool.disk_path`, a read-only mount of the host
filesystem backing Docker and attempt workdirs. It adds to existing PSI admission:

- Only one cold start per 30-second warmup period while other attempts run. An empty
  pool may immediately start one attempt. The existing 15-second calm cooldown also
  applies after a refusal.
- New work needs headroom for a conservative build estimate: the largest observed
  working set per active attempt, at least a quarter of MemoryHigh or 512 MiB, plus
  25%. Samples continue when all slots are occupied. Estimates reset on restart;
  cold-start pacing applies again. There is no learned cross-host model.
- Claims pause below 10% free space (at least 2 GiB) or 5% free inodes. They resume
  above 15% free space (at least 3 GiB) and 10% free inodes. Unreadable filesystem
  statistics refuse new claims. Root's reserved blocks are not counted as available.

Pending tasks remain in Multica. Admission does not kill active attempts, prune
unrelated Docker resources, or introduce a second queue. Clean workdirs can use the
existing no-sessions path; deployments that retain sessions keep the existing GC.

## Alternatives

Fixed per-role budgets preserve unnecessary coupling between identity and capacity.
An unbounded concurrency setting plus PSI still admits a cold burst. A second task
scheduler duplicates Multica authority. Global Docker prune may delete unrelated
workloads and cannot provide an execution disk quota.

## Consequences

One attempt can use the shared envelope, and new starts slow down under pressure.
Late memory growth can still reach MemoryMax and fail a task; admission protects
new claims, not every future allocation. Watermarks are not a disk quota: an active
build can fill the disk. Swap can reduce throughput and does not replace RAM.
The envelope must include all protected services; deploying additional workloads
requires rerunning host sizing. Multi-host scheduling remains outside this change.

Tests cover cold bursts, warmup, observed headroom, disk and inode hysteresis,
unreadable mounts, conflicting CPU modes and unchanged legacy defaults. Host
sizing and real compiler/nested-engine pressure require deployment acceptance.
