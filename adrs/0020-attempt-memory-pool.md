# ADR 0020: Shared attempt memory pool

Status: Accepted
Date: 2026-10-12

## Context

Attempts have a fixed memory limit without swap (`opencode.resources.memory`, 4 GiB by
default). A .NET build with Testcontainers peaks at about 1.2 GiB of working memory
(ADR 0019), but compilers, test hosts and inner databases spike: a 1 GiB limit
OOM-killed the Roslyn server three times. A fixed limit wastes the memory idle
attempts reserve, and an attempt that reaches it dies. Multica retries `timeout` and
`runtime_*` failures automatically, so a task that is killed for memory and reported
with such a reason would run, die and rerun.

Ten attempts at 4 GiB need 40 GiB of limits for about 12 GiB of working memory.
Overcommitting fixed limits moves the kill to the host's OOM killer, which may choose
the engine or the controller.

## Decision

Attempts may share one host cgroup slice, the pool, instead of fixed limits.

- **Host.** A systemd slice (or cgroupfs directory) with `MemoryHigh` (soft limit:
  the kernel reclaims and swaps, attempts slow down), `MemoryMax` (last resort) and
  `MemorySwapMax`; the host has swap. dockerd, containerd, Sysbox and the controller
  stay outside the slice with negative OOM score adjustments.
- **Configuration.** `opencode.pool: {cgroup_parent, cgroup_dir, attempt_memory_max,
  max_pressure, max_working_set}`. `cgroup_parent` is Docker's `--cgroup-parent`;
  `cgroup_dir` is the slice directory mounted read-only into the controller. Without
  `pool`, fixed limits stay as before; with it `resources.memory` is refused.
- **Attempts.** `--cgroup-parent` and no memory limit, so swap is bounded only by the
  pool. `attempt_memory_max` (a size or a percentage of the pool's `memory.max`) adds a
  per-attempt ceiling with `--memory-swap=-1`, rounded down to a MiB so `memory.max`
  keeps it exactly. The policy compares `CgroupParent`,
  `Memory` and `MemorySwap` with the configuration; after start the controller finds
  the attempt cgroup under `cgroup_dir` and compares `memory.max` and `memory.swap.max`.
- **Preflight.** The parent must fit the engine's cgroup driver, and the slice must set
  `memory.high` and `memory.max` with high not above max. Otherwise startup fails.
- **Admission.** Before each claim the controller reads the slice: PSI `memory.pressure`
  "some" `avg10` above `max_pressure` (default 10 %) or a working set (`memory.current`
  without inactive file cache) above `max_working_set` of `memory.high` (default 0.9)
  defers claims; an unreadable slice too. Claims resume after 15 s without a refusal:
  under load the working set hovers at `memory.high`. Tasks stay queued in Multica; the
  log records each pause and resume once.
- **Removal.** Killed processes throttled at `memory.high` and swapped out exit slowly;
  a stand run took over 30 s per attempt. Removal gets three minutes of its own,
  independent of the caller's cleanup deadline: a failed cleanup restarts the controller,
  and Multica retries every task in flight as `runtime_recovery`.
- **Out of memory.** When the agent command fails or its stream ends early (a killed
  OpenCode leaves a step open, which Multica would retry as a provider stream cut),
  the controller reads the attempt's
  `memory.events`, `memory.max` and, from Linux 5.19, `memory.peak` (through the pool on the host,
  otherwise inside the attempt, otherwise Docker's `OOMKilled`). An `oom_kill` makes the
  failure an `OOMError`, reported with the canonical `agent_error.process_failure`:
  outside Multica's automatic retries and resume-safe. The message names the killed
  process count, the peak and the limit reached (attempt ceiling or pool) and that the
  task is not retried.

## Consequences

Memory is shared by actual use: a spike slows attempts and swaps instead of killing,
and new work waits while the pool is under pressure. On a 6 GiB VM with a 2/3 GiB
pool and 4 GiB swap, four parallel .NET Testcontainers attempts (PostgreSQL and MongoDB
in nested engines) held the pool at `memory.high` with up to 3.7 GiB in swap and PSI
"some" up to 22 %, without an OOM kill; a run took 45–67 s instead of 40 s. A task can still die when the
pool reaches `MemoryMax` or its own ceiling; it then fails once with an explicit
message. Admission bounds only new claims: attempts already running can push the pool
into `MemoryMax`, so `MemoryHigh` must leave headroom, and the controller's concurrency
remains the upper bound. Tasks waiting under sustained pressure can expire in the
Multica queue. Sizing the slice, swap and its disk are host decisions
(`examples/sysbox-host`). The controller reads host cgroup files, which a stopped and
restarted slice invalidates until the controller restarts. Tests: unit tests of the
pool files (limits, PSI, placement, memory events), configuration, admission in the
claim loop and the failure mapping; integration tests of OOM classification (runc) and
of sysbox attempts placed in a pool; a live run with parallel attempts in a small pool.

## References

- [Control Group v2: memory](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)
- [PSI](https://docs.kernel.org/accounting/psi.html)
- ADR 0006 (fixed limits), ADR 0019 (nested Docker).
