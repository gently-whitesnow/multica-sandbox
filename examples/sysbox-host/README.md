# Sysbox host for nested Docker

Recipe behind [ADR 0019](../../adrs/0019-nested-docker-under-sysbox.md). Docker Desktop
runs Sysbox only inside Enhanced Container Isolation, so macOS contributors use a Lima
VM; on a Linux host run `provision.sh` directly (it needs a host without containers).

| File | Purpose |
|---|---|
| `lima.yaml` | Ubuntu 24.04 arm64 on `vz`, 6 CPUs / 6 GiB / 50 GiB, Rosetta only for amd64 builds, no host directory mounts, only `docker.sock` forwarded |
| `provision.sh` | Pinned Docker CE and containerd (key checked by fingerprint), containerd image store, Sysbox CE 0.7.1 by release sha256, `sysbox-mgr --relaxed-read-only` |
| `engine.Dockerfile` | Minimal attempt image with a nested engine, built by the integration test |
| `up.sh` | `up` (create or start, add the `lima-sysbox` context), `stop`, `delete` |

```bash
examples/sysbox-host/up.sh          # about 3 minutes from scratch
DOCKER_CONTEXT=lima-sysbox VERIFY_SYSBOX=1 go test -tags containers -count=1 -run Sysbox ./internal/docker
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

Pitfalls:
- Lima renders provision scripts as Go templates: double braces in `provision.sh`
  break provisioning silently, so it parses JSON with `jq`.
