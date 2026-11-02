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
  applies after a pressure, headroom or disk refusal. A warmup wait is pacing, not a
  pause: it neither restarts the cooldown nor sets `sandbox_claims_paused`.
- New work needs headroom for a conservative build estimate plus 25%: the peak
  observed working set per active attempt, halved every 10 minutes after it was last
  seen, and at least a cold floor. The floor is `pool.attempt_memory_max` (else the
  legacy 4 GiB attempt limit), at most a quarter of MemoryHigh and at least 512 MiB,
  so it does not grow with the host. Samples continue when all slots are occupied.
  Estimates reset on restart; cold-start pacing applies again. There is no learned
  cross-host model.
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

Tests cover cold bursts, warmup, decaying observed headroom, the ceiling-based floor, disk and inode hysteresis,
unreadable mounts, conflicting CPU modes and unchanged legacy defaults. Host
sizing and real compiler/nested-engine pressure require deployment acceptance.

Refinement (ECO-149, 2026-11-02). On vps-worker attempts ran one at a time at
`concurrency: 3`. The working set was `memory.current` without inactive file cache,
and the estimate divided it by the active attempts. Page cache stays charged to the
slice after attempts and their nested engines exit: with no attempt running the slice
held 2.5 GiB of file cache, 0.7 GiB of it active, and no anon or shmem. With one
attempt that cache entered both occupancy and the per-attempt estimate, so every
second start was refused as `build headroom below estimate`, followed by the calm
cooldown.

- The working set, for admission, the estimate, `max_working_set` and the pool usage
  metric, is the memory the kernel cannot reclaim without swapping or killing:
  `memory.current` minus file cache other than shmem and minus reclaimable slab, that
  is anon, shmem (tmpfs, `/dev/shm`) and other kernel memory (stacks, page tables,
  sockets, unreclaimable slab). The subtraction keeps a `memory.stat` key that is
  missing counted as used.
- The estimate samples the largest attempt cgroup in the slice (`docker-<id>.scope`
  or `<id>`), not the slice divided by active attempts. A heavy build is not averaged
  with light ones, a claimed attempt whose container does not exist yet does not
  dilute the sample, and memory left in the slice counts in occupancy only.

PSI stays the guard for cache: when admitted work makes the kernel reclaim hot cache
at MemoryHigh, refaults and stalls raise `memory.pressure` and pause claims. Warmup,
the floor, the 25% margin, disk watermarks and MemoryHigh/MemoryMax are unchanged.
Counting active file cache would again refuse starts on reclaimable memory; anon
alone would miss tmpfs and kernel memory. Tests replay the vps-worker slice: the
second and third attempts start after warmup, large anon, shmem or kernel memory
still refuses, and PSI pauses claims. Throughput on the host requires deployment
acceptance.
