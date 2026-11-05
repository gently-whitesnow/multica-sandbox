# ADR 0035: Workspace reference mirrors for checkouts

Status: Proposed. Off unless `opencode.mirrors` is configured; enabling it awaits
owner review of the threat model below.
Date: 2026-11-04

## Context

Each new session volume clones its repositories in full through the Git relay
(ADR 0015), which loads the 256 MiB controller and stores a copy per session. The
native daemon (Multica v0.7.2 `daemon/repocache`, `daemon/gc.go`) keeps a bare clone
per (workspace, repository), fetches it on every checkout under a per-repository
mutex, adds a worktree or hard-linked clone per task, and evicts caches unused for
30 days. The controller image has no `git`; attempts run untrusted code as uid 65532;
mounts are fixed per workspace at attempt creation.

## Decision

**Keying.** One bare mirror per (owner, workspace, repository), named by the SHA-256
of the Git relay's key: host lower-cased, path case kept, no trailing slash or `.git`,
so `https`, `ssh` and scp-like remotes share it. Each workspace has one owner-labelled
volume (`io.multica-sandbox.mirror=<workspace>`).

**Mount.** Attempts with the helper and a workspace mount their workspace's volume
read-only at `/opt/multica-sandbox/mirrors`; the policy check pins it. The attempt can
read every workspace mirror, no wider than the relay serves it (ADR 0015).

**Refresh.** A checkout records the use first, as natively. Without a record it waits
up to `wait` for the first refresh; otherwise it uses the mirror as it is and starts a
background refresh when the last is older than `refresh`. One refresh per mirror runs
at a time, bounded by `timeout`, in a container from the agent image under runc (uid
65532, read-only root, no capabilities, 1 CPU, 1 GiB) on its own attempt-style network
with a workspace Git relay grant. It runs only `git init --bare` and `git fetch
--prune` of branches and tags by URL, without hooks or system config; `FETCH_HEAD` is
removed, so the mirror holds neither the grant nor the URL. Below the claim pause
watermark no new mirror starts.

**Recovery.** A first fetch or rebuild goes to `<key>.new` and replaces the mirror when
done. A refresh removes lock files left by a cut one. After a failed fetch, `git fsck
--connectivity-only` keeps a readable mirror and rebuilds an unreadable one.

**Clone.** `checkout.sh` clones new checkouts with `--reference-if-able <mirror>
--dissociate` and falls back to a plain clone; refs come from origin. A checkout holds
its mirror until its clone ends; afterwards it does not depend on it.

**Collection.** The session sweep (ADR 0034) collects mirrors, so `mirrors` requires
`sessions`:
- every `sessions.interval` the helper removes mirrors unused for `ttl` and measures
  the rest; a mirror without a record gets one, as natively;
- below the claim pause watermark, after session artifacts, idle session volumes and
  idle mirrors go in one least recently used order until the resume watermark;
- `sessions.budget` counts measured mirror bytes, including mirrors in use, and
  removes in the same order.
`sandbox-helper drop` removes only named mirrors with their `.new` and `.old`, as uid
65532 without network or capabilities, and reports each remaining mirror's bytes.
Mirrors in use, refreshing or being removed are skipped. Collections log
`mirrors_removed` and `mirror_bytes` and count `sandbox_mirrors_removed_total` and
`sandbox_mirrors_freed_bytes_total` by trigger.

**Configuration.** `opencode.mirrors` requires `git_relay` and `sessions`; `{}` sets
`refresh` 15m, `wait` 2m (below the 5-minute checkout limit), `timeout` 10m and `ttl`
720h, the native values.

### Native comparison

| Concern | Native | Here |
|---|---|---|
| Key | workspace + normalized URL | same, per owner |
| Locking | fetch and worktree add serialized | single-flight refresh; dissociate and fallback cover overlap |
| Broken cache | partial clone removed; no fallback | `.new`, stale locks, `fsck` rebuild, plain fallback |
| Fetch | every checkout and at registration; 10m | every `refresh`, background; 10m; `--prune` |
| LFS, submodules | not handled | not handled; checkouts fetch them from origin |
| Credentials | a token in the URL persists in the cache | grant in the environment only |
| Task dependency | worktrees; GC skips live ones | none after the clone |
| Age | 30 days; missing stamp backfilled | 720h; missing record backfilled |
| Disk pressure | none | watermarks and budget, one LRU with sessions |

### Deliberate differences

- **Isolation:** only controller-owned refresh containers write mirrors, per
  workspace, because attempts run untrusted code with the agent image's `git`.
- **Read-only mount and `--dissociate` instead of worktrees:** a worktree would give
  the attempt write access to the shared repository and bind the session to it.
- **Relay:** the grant keeps credentials out of mirrors and applies the relay's host
  policy, as for attempts.
- **Throttled refresh, no registration sync:** checkouts still fetch from origin, so a
  mirror only saves bytes; the controller learns repositories at checkout.
- **Pressure:** mirrors share the host with sessions and engines and yield like other
  rebuildable data.

### Threat model

- Only refresh containers write mirrors, from the upstream through the relay;
  `index-pack` verifies received objects; no attempt input reaches them beyond the
  admitted URL.
- A malicious repository only fills its own bare mirror: no checkout, no hooks.
- Keying and mounts are per workspace; a mirror equals what a plain clone would fetch.
- Git treats unreadable mirror objects as missing and fetches them from origin
  (tested), so a damaged mirror costs only the saving.

## Consequences

- New sessions fetch only objects newer than the mirror. A large first refresh that
  outlasts `wait` clones twice once.
- Sizes lag up to one sweep in the budget; watermarks follow the real filesystem.
- Clearing stale locks needs `find` in the agent image (Debian bases have it).
- Tests pin keying, single-flight, leases, low-disk refusal, fallbacks, stale locks,
  rebuild, credential absence, collection by age, pressure and budget, and the helper.
  `VERIFY_CONTAINERS` runs the refresh container with a `git` stand-in, the read-only
  mount and the helper on real volumes; real `git` in a refresh container through a
  live relay is not yet verified.
