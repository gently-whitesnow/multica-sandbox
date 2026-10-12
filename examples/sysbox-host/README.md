# Sysbox host for nested Docker

Recipe and smoke behind [ADR 0019](../../adrs/0019-nested-docker-under-sysbox.md). Docker
Desktop runs Sysbox only inside Enhanced Container Isolation, so macOS contributors use a
Lima VM; on a Linux host run `provision.sh` directly (it needs a host without containers).

| File | Purpose |
|---|---|
| `lima.yaml` | Ubuntu 24.04 arm64 on `vz`, 6 CPUs / 6 GiB / 50 GiB, Rosetta only for amd64 builds, no host directory mounts, only `docker.sock` forwarded |
| `provision.sh` | Pinned Docker CE and containerd (key checked by fingerprint), containerd image store, Sysbox CE 0.7.1 by release sha256, `sysbox-mgr --relaxed-read-only` |
| `up.sh` | `up` (create or start, add the `lima-sysbox` context), `stop`, `delete` |
| `smoke.sh`, `smoke.Dockerfile` | The attempt policy under sysbox-runc reproduced with `docker create` |
| `dind` | Prototype of the planned `sandbox-helper dind` supervisor |

```bash
examples/sysbox-host/up.sh       # about 3 minutes from scratch
examples/sysbox-host/smoke.sh    # 1–2 minutes
examples/sysbox-host/up.sh stop
```

Pitfalls:
- Lima renders provision scripts as Go templates: double braces in `provision.sh`
  break provisioning silently, so it parses JSON with `jq`.
- The Sysbox post-install script restarts Docker, fails if any container exists and adds
  `bip` and `default-address-pools` to `daemon.json`. Restart Sysbox only without
  Sysbox containers.
- Without `--relaxed-read-only`, read-only attempts get read-only cgroup and
  `/var/lib/docker`, and the inner engine cannot start containers.
