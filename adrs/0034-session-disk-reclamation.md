# ADR 0034: Session disk reclamation

Status: Proposed.
Date: 2026-11-04

## Context

Retained session volumes (ADR 0015) hold HOME with Go, npm and NuGet caches, clones
and build outputs, without a size bound. Only the sweep every `interval` (2 h)
collected them, by TTL, count and closed issues. Disk admission (ADR 0028) paused
claims below its watermark, but nothing freed space then: a deployment spent about
half its time paused on disk. The native daemon removes build artifacts of finished
workdirs after 12 h and whole workdirs later.

## Decision

- **Artifacts.** The sweep prunes volumes idle for `sessions.artifact_age` (default
  `12h`, upstream's): `node_modules`, `.next`, `.nuxt`, `.turbo` in checkouts, .NET
  `bin`/`obj` beside a `*.csproj`/`*.fsproj`/`*.vbproj`, the HOME caches `.cache`,
  `.npm`, `.nuget/packages`, `go/pkg/mod` and the TMPDIR every attempt empties.
  `dist`, sources, `.git` and the session database stay.
- **Helper.** A short `sandbox-helper prune` container deletes them like the volume
  init: runc, no network, read-only rootfs, no capabilities, uid 65532, 64 MiB and
  16 pids, the owner label. It never follows symlinks, leaves the volume filesystem
  or enters `.git`; build directories are searched eight levels deep. It reports
  deleted and remaining bytes. A record beside the last-use mark skips the volume
  until its next use.
- **Pressure.** With `pool.disk_path`, the sweep reads the disk every minute. Below
  the claim pause watermark it prunes, then removes, idle volumes least recently
  used first until the resume watermark (the same functions as admission).
- **Budget.** Optional `sessions.budget` (a size such as `40g`) bounds the volumes'
  measured bytes: the sweep measures idle volumes used since their last record
  (`sandbox-helper du`) and removes the least recently used idle ones beyond it,
  counting busy ones at their last size. It is off by default: the watermarks
  follow the real filesystem, and a fixed size is host-specific.
- Busy volumes are never touched; a volume is reserved while pruned or removed.
  Each collection logs `session volumes reclaimed` (trigger, removed, pruned,
  pruned and freed bytes) and counts `sandbox_sessions_removed_total` and
  `sandbox_sessions_pruned_bytes_total` by trigger: lifetime, age, budget, pressure.

## Alternatives

`docker system df -v` sizes every image, container and volume, prints rounded
human sizes and allows one run at a time; the helper measures one volume exactly.
Removing whole volumes first loses resumable sessions that a prune keeps.

## Consequences

A follow-up after a prune reinstalls dependencies; one arriving during a prune
waits 15 s, then runs in a fresh volume. Pruned bytes count file blocks, so hard
links overcount, and freed bytes are approximate under concurrent writes. A
checkout that commits `node_modules` shows deletions after a prune, as natively.
Without the new helper image prunes fail and pressure falls back to removal.
Tests pin the prune list, symlinks, read-only module caches, the depth bound,
pressure order, records, the budget and busy volumes; real volumes need
`VERIFY_CONTAINERS`.
