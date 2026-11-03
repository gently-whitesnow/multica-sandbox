# ADR 0015: Repository workdirs, mediated Git and session resume

Status: Accepted
Date: 2026-10-08

## Context

Upstream Multica (v0.5.2) checks out lazily. The claim lists `repos[{url, ref}]`.
The agent runs `multica repo checkout`, which posts to the daemon's
`127.0.0.1:$MULTICA_DAEMON_PORT/repo/checkout` with the `mat_` bearer. The daemon
clones with host credentials into `<workdir>/<repo>` on the branch
`agent/<agent>/<task12>` and keeps checkouts that hold work. Resume reruns
`opencode run --session <prior_session_id>` in the same `prior_work_dir`, relying
on the host's persistent OpenCode store. Agents push and open PRs with host `git`
and `gh` credentials; no server API issues Git credentials.

Our attempts are offline, with a 256 MB tmpfs workspace and a fresh session each
time. ADR 0001 requires explicit, exclusive, retained state for resume. ADR 0003
requires mediated external Git. The #48 spike (Docker Engine 29.2.1, OpenCode
1.18.35) established these facts:
- `OPENCODE_DB` moves only the session database out of `$XDG_DATA_HOME/opencode`
  (which also holds auth stores); a new container resumed a session from a volume
  holding just the workdir and that database.
- A Git smart HTTP reverse proxy can swap credentials, restrict repositories and
  check `git-receive-pack` refs before streaming the pack.
- The unchanged upstream CLI reaches a controller endpoint through an
  image-mounted loopback forwarder run as the attempt user.
- The official OpenCode image has no `git`.

## Decision

**Workdir volume.** OpenCode attempts mount a controller-owned named volume as
the workdir at `/workspace/work`. Its subdirectory `.multica-sandbox` holds the
session database (`OPENCODE_DB`). Projected configuration, the prompt, XDG
config, data and state directories and native auth stores stay in the per-attempt
tmpfs. Refinement (2026-10-30): caches filled the tmpfs, which also counts as
memory, so `HOME` (with `XDG_CACHE_HOME`) and a per-attempt `TMPDIR` live under
`.multica-sandbox` on the volume, never shared between workdirs. OpenCode
receives them by explicit paths, so retained state never contains attempt
credentials. A one-shot init container prepares each volume: no network, a
read-only rootfs, only `CAP_CHOWN`, `chmod 0700`, then `chown 65532`. Labels
bind a volume to its controller, workspace, agent and issue. Fresh runs get a
per-attempt volume that is removed at cleanup.

**Sessions.** With `sessions` configured, issue tasks retain one volume per
(controller, workspace, agent, issue), named from those IDs, with one writer at a
time: a second run waits 15 s, as upstream waits for its workdir lock, then gets
a per-attempt volume. Reports carry the native `session_id` and a `work_dir`
naming the volume. `prior_session_id` is honoured only when `prior_work_dir`
names that volume and it already existed with matching labels. Otherwise the run
starts fresh with the upstream continuity notice. A refused resume runs fresh
once (ADR 0023) and reports `retired_session_id`.
`session_rollout_missing` is reported only when no session is retained. Done or
cancelled issues (upstream GC), an idle TTL and a session cap bound retention,
swept periodically. Disk quotas are deployment-owned: the Docker `local` driver
cannot limit volumes.

**Checkout endpoint.** The controller serves the upstream `/repo/checkout`
request and response contract on the attempt network. A static forwarder,
image-mounted at `/opt/multica-sandbox/helper`, listens on
`127.0.0.1:$MULTICA_DAEMON_PORT` as the attempt user. The controller authorizes
each request by:
- the attempt's opaque Multica relay credential;
- the claim's task and workspace;
- the claim's or, refreshed, the workspace's repositories;
- a workdir inside `/workspace/work`.

It then runs `git` through `docker exec` as uid 65532 with the image's `git`,
reproducing upstream path, branch, base-ref, kept and `fresh` semantics and the
`info/exclude` entries. Credentials never enter a context that executes
agent-writable hooks or configuration. Repository tasks therefore require `git`
in the agent image.

**Git relay.** A controller relay mediates Git smart HTTP with a per-attempt
opaque credential. The attempt receives it only through `GIT_CONFIG_*`
environment entries that route HTTPS and scp-style remotes to the relay (ADR 0016).

`origin` keeps the real URL. The relay attaches controller-only, workspace-scoped
credentials per Git host and serves the attempt's workspace hosts. Grants end at
cleanup and on controller restart. Credential issuance and rotation (GitHub App
tokens) stay external, as files the controller rereads.

Rejected alternatives:
- Credentialed checkout in the controller or a sidecar: agent hooks and
  configuration would run with the credentials.
- Persisting the OpenCode data directory (auth stores); host bind mounts (the
  controller runs in a container); a sidecar in the attempt's network namespace.
- SSH agent forwarding; hosting a Git service; pre-cloned mirrors (#5).

Refinement (#50, 2026-10-08). The helper is a separate digest-pinned image (Dockerfile
target `helper`, config `helper`); it also runs the volume init. Attempts get
the workdir volume only with the helper. The checkout route shares the Multica relay
listener. Checkout follows upstream isolated mode with a fresh clone.

Refinement (#52, 2026-10-08). Push follows the native runtime, not an
`agent/<agent>/*` ref policy, so the relay adds no ref checks. Checkouts set a per-host commit identity (default: agent
name) and upstream's Co-authored-by hook, following the setting polled every 10 s (#61).

Refinement (#55, 2026-10-08). Pull requests use the unchanged `gh`. Attempts
resolve forge API names to loopback, where the helper forwards 443 to a controller TLS
relay with a name-constrained per-process CA. The relay swaps the Git relay credential,
sent as the `gh` token, for the workspace token.

Refinement (#81, 2026-10-30). Project resources follow upstream: Project Context
and `.multica/project/resources.json`. As upstream `ensureRepoReady`, checkout outside
the claim rereads workspace repositories; aliases cover all bound hosts. A `local_directory` pinned here rejects the claim.

Refinement (2026-11-03). As natively, the relay serves any bound-host repository the
credential allows; per-repository grants bounded Git alone, not the forge API. Checkout
keeps upstream's workspace check. The global Git config holds the hosts' agreed commit
identity, else the agent default.

## Consequences

Agents work on repositories and continue sessions without credentials in the
attempt. Retained volumes are part of the trust boundary. They are reused
only for the same workspace, agent and issue, and must not be returned to any
clean pool (ADR 0004). Each session pays for its first clone. The relay bounds
pushes to the workspace's hosts; deployment-owned credential scope and forge
branch protection bound repositories and refs.

Implementation follows #47. Kubernetes (#6) needs equivalent volumes and a
loopback-capable helper.
