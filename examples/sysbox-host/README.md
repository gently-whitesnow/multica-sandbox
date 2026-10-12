# Sysbox host for nested Docker

Recipe behind [ADR 0019](../../adrs/0019-nested-docker-under-sysbox.md) (nested Docker)
and [ADR 0020](../../adrs/0020-attempt-memory-pool.md) (shared memory pool). Docker
Desktop runs Sysbox only inside Enhanced Container Isolation, so macOS contributors use a
Lima VM; on a Linux host run `provision.sh` and `pool.sh` directly.

| File | Purpose |
|---|---|
| `lima.yaml` | Ubuntu 24.04 arm64 on `vz`, 6 CPUs / 6 GiB / 50 GiB, Rosetta only for amd64 builds, no host directory mounts, only `docker.sock` forwarded |
| `provision.sh` | Pinned Docker CE and containerd (key checked by fingerprint), containerd image store, Sysbox CE 0.7.1 by release sha256, `sysbox-mgr --relaxed-read-only` |
| `pool.sh`, `multica_attempts.slice` | Swap file and the attempt pool slice: `MemoryHigh`, `MemoryMax`, `MemorySwapMax` |
| `compose.pool.yaml` | Controller override: read-only slice mount for `pool.cgroup_dir`, `oom_score_adj` |
| `engine.Dockerfile` | Minimal attempt image with a nested engine, built by the integration test |
| `up.sh` | `up` (create or start, add the `lima-sysbox` context), `pool`, `stop`, `delete` |

```bash
examples/sysbox-host/up.sh          # about 3 minutes from scratch
examples/sysbox-host/up.sh pool     # 4 GiB swap, 2G/3G/4G pool for the 6 GiB VM
DOCKER_CONTEXT=lima-sysbox VERIFY_SYSBOX=1 VERIFY_SYSBOX_POOL=multica_attempts.slice \
  go test -tags containers -count=1 -run 'Sysbox|OutOfMemory' ./internal/docker
examples/sysbox-host/up.sh stop
```

Host requirements for `runtime: sysbox-runc`:
- Sysbox CE 0.7.1 installed on a host without containers: its post-install script
  restarts Docker and adds `bip` and `default-address-pools` (172.25.0.0/16 in /24
  networks, one per attempt) to `daemon.json`. Restart Sysbox only without Sysbox containers.
- `sysbox-mgr --relaxed-read-only`; without it read-only attempts get a read-only cgroup
  and `/var/lib/docker`, and the controller's canary refuses to start.
- `/var/lib/sysbox` on its own filesystem: inner images and containers have no quota
  (1.6 GB for PostgreSQL and MongoDB images).

Host requirements for `pool`:
- Swap, so the pool can slow down instead of killing (`pool.sh` adds a swap file).
- The slice with `MemoryHigh` below `MemoryMax`; attempts get no own limit unless
  `pool.attempt_memory_max` sets one. Under the systemd cgroup driver the controller's
  `pool.cgroup_parent` is the slice name, under cgroupfs an absolute path.
- dockerd, containerd and Sysbox outside the pool with negative `OOMScoreAdjust`
  (package defaults -500, -999, -500); the controller with `oom_score_adj` (`compose.pool.yaml`).
- Sizing: one .NET Testcontainers attempt alone fills a 2 GiB `MemoryHigh` with page cache
  and tmpfs; plan the working set per heavy attempt at 1–1.5 GiB. Removing a Sysbox
  attempt rewrites its root and inner data (IO pressure up to 47 % in the VM): keep swap
  and `/var/lib/sysbox` on fast disks, and leave memory outside the pool for the engine.

Pitfalls:
- Lima renders provision scripts as Go templates: double braces in `provision.sh`
  break provisioning silently, so it parses JSON with `jq`.
- The bind-mounted slice directory goes stale if the slice is stopped: restart the
  controller after `systemctl restart` of the slice.
