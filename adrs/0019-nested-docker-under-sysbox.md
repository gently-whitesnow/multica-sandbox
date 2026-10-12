# ADR 0019: Nested Docker under sysbox-runc

Status: Proposed
Date: 2026-10-12

## Context

Agents run integration tests with Testcontainers and `docker compose` (databases,
message brokers, browsers). Attempts have no Docker socket (ADR 0006), and the host
socket would be root on the host. A privileged Docker-in-Docker container is the same.
ADR 0006 named Sysbox v0.7.1 as the maintained candidate: each container gets its own
user namespace, and a Docker engine inside it runs without host privileges.

A spike reproduced the attempt policy with `docker create` against Sysbox CE 0.7.1 on
Docker Engine 29.9 (containerd image store, cgroup v2, Ubuntu 24.04 arm64 VM; the
recipe is `examples/sysbox-host`). Findings:

- `--read-only` breaks nested Docker by default: Sysbox mounts `/sys/fs/cgroup` and
  the implicit `/var/lib/docker` read-only, and the inner runc fails with
  `mkdir /sys/fs/cgroup/docker: read-only file system`. `sysbox-mgr
  --relaxed-read-only` keeps the root filesystem read-only and makes cgroup and
  `/var/lib/{docker,kubelet,buildkit}` writable.
- The inner `dockerd` must run as root of the user namespace (PID 1 with the
  container `--user 0:0`). `uid_map` is `0 100000 65536`, never the identity map.
- Sysbox CE ignores `--cap-drop=ALL` for PID 1 (`CapEff=000001ffffffffff` inside the
  namespace). `SYSBOX_HONOR_CAPS=TRUE` honours it, and then `dockerd` cannot chown
  its socket. A `docker exec --user 65532:65532` process has `CapEff=0`, `CapPrm=0`,
  `CapAmb=0` and `NoNewPrivs=1`.
- Under sysbox-runc, `--workdir=/workspace` recreates the tmpfs mount point as `0755`
  root; runc keeps `mode=1777`. The supervisor must restore the mode.
- `no-new-privileges`, private cgroup and IPC namespaces, the internal attempt
  network, image mounts and the workdir volume initialised by a runc container all
  work unchanged. Inner containers, including `--privileged`, stay inside the
  namespace.
- Without network, inner pulls work only through `registry-mirrors` pointing at a
  pull-through registry peer. Mirrors cover Docker Hub only; other registries need
  their own proxy repositories or allowed egress.
- `docker rm -fv` removes the attempt; sysbox-mgr deletes the inner data
  asynchronously in about one second. An image with `VOLUME /var/lib/docker`
  receives an anonymous Docker volume instead of the sysbox-mgr mount.

Measurements (one attempt, 4 CPUs, .NET 10 with Testcontainers PostgreSQL 17 and
MongoDB 8, Ryuk disabled, plus a compose stack and a 12×400-class build):

| Metric | Result |
|---|---|
| Inner `dockerd` ready after start | 0.45–0.9 s, no systemd |
| Test run, cold / warm inner images | 30 s / 23 s (MongoDB readiness dominates) |
| Peak working memory (anon, shmem, kernel) | 1.2 GiB; tmpfs caches 0.4–0.5 GiB of it |
| 1 GiB limit | three OOM kills of the Roslyn server; build twice as slow |
| Peak processes | 234 |
| Inner `/var/lib/docker` | 1.6 GB for the two database images; no quota |

## Decision

Add `sysbox-runc` as an explicit, opt-in runtime of the Docker backend.

- **Configuration.** `opencode.runtime: runc | sysbox-runc` (default `runc`) and,
  for sysbox only, `opencode.dind: {registry_mirrors, insecure_registries,
  ready_timeout}`. Resources stay under `opencode.resources`; nested Docker needs
  more processes and files than the defaults (`pids` 2048, `nofile` 65536) and
  4–6 GiB of memory for .NET with databases.
- **Preflight.** `docker info` lists `sysbox-runc`. A canary attempt with the real
  policy must show a non-identity `uid_map`, writable `/sys/fs/cgroup` and
  `/var/lib/docker` (detects a host without `--relaxed-read-only`) and an exec
  process with `CapEff=0` and `NoNewPrivs=1`. Any failure refuses startup.
- **Attempt.** `--runtime` from configuration; for sysbox the container runs as
  `0:0` with an extra `/run` tmpfs (64 MiB) for the socket and engine state. The
  entrypoint becomes `sandbox-helper dind` instead of `sleep`. `--read-only`,
  `--cap-drop=ALL`, no-new-privileges and private namespaces stay.
- **Every `docker exec` sets `--user 65532:65532`.** Under runc this is a no-op;
  under sysbox it is what keeps the agent unprivileged.
- **Policy.** The inspected runtime, user and tmpfs set must match the
  configuration. After start, the controller reads `/proc/self/status` and
  `/proc/self/uid_map` of an exec process; this replaces trust in `CapDrop`,
  which Sysbox CE does not apply to PID 1.
- **`sandbox-helper dind`.** PID 1: reaps children and forwards signals, restores
  the `/workspace` mode, starts `dockerd` on `/run/docker.sock` with the agent
  group, the configured mirrors and `/run` exec root and pid file, waits for the
  API and writes a readiness marker. The controller waits for it before the agent
  starts; a timeout is `environment_prepare_failed`.
- **Agent environment.** `DOCKER_HOST` stays unset (`/var/run` resolves to `/run`).
  `TESTCONTAINERS_RYUK_DISABLED=true` is configuration, not a constant: the
  attempt's removal cleans up anyway.
- **Image contract (sysbox).** The image provides `dockerd`, `containerd`, `runc`
  and `iptables` and declares no volumes; the image probe checks `dockerd --version`.
- **Cleanup and reconciliation** stay unchanged.

## Consequences

Hosts need Sysbox CE 0.7.1, installed on a host without containers (its post-install script
restarts Docker and adds `bip` and `default-address-pools` to `daemon.json`).
`sysbox-mgr` runs with `--relaxed-read-only`. `/var/lib/sysbox` gets its own
filesystem: inner Docker data has no quota. Sysbox CE maps all containers into one
uid range.

Agents get a private Docker engine per attempt that disappears with the attempt.
The trust boundary becomes the user namespace: root inside the attempt, including
inner `--privileged`, is unprivileged on the host, but kernel exposure is larger
than under runc. Disk use per attempt is unbounded by Sysbox and must be bounded
by the host. Tests: unit tests of `createArgs` and the policy for both runtimes;
`VERIFY_SYSBOX=1` integration on a Sysbox host covering readiness, exec
capabilities, mirror pulls, inner runs, read-only root, cleanup and adversarial
cases (inner `--privileged`, rootfs writes, the host socket).

## References

- [Sysbox v0.7.1](https://github.com/nestybox/sysbox/releases/tag/v0.7.1)
- [Sysbox security: capabilities and read-only containers](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/security.md)
- `examples/sysbox-host`: VM recipe and smoke of the attempt policy.
