# ADR 0015: Repository workdirs, mediated Git and session resume

Status: Accepted
Date: 2026-10-08

## Context

Upstream Multica (`b4ca5b4`) checks out lazily. The claim lists `repos[{url, ref}]`.
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
- `OPENCODE_DB` moves only the session database out of `$XDG_DATA_HOME/opencode`,
  which also holds `auth.json` and `mcp-auth.json`. A new container resumed a
  session from a volume holding just the workdir and that database, and the
  volume held no projected secrets.
- A Git smart HTTP reverse proxy can swap credentials, restrict repositories and
  check `git-receive-pack` refs before streaming the pack.
- The unchanged upstream CLI reaches a controller endpoint through an image-mounted
  loopback forwarder started as the attempt user.
- The official OpenCode image has no `git`.

## Decision

**Workdir volume.** OpenCode attempts mount a controller-owned named volume as
the workdir at `/workspace/work`. Its subdirectory `.multica-sandbox` holds the
session database (`OPENCODE_DB`). Projected configuration, the prompt, `HOME`,
XDG directories and native auth stores stay in the per-attempt tmpfs. OpenCode
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
starts fresh with the upstream continuity notice. A resume that yields no session
and no tool use is retried fresh once and reports `retired_session_id`.
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
- the claim's repositories;
- a workdir inside `/workspace/work`.

It then runs `git` through `docker exec` as uid 65532 with the image's `git`,
reproducing upstream path, branch, base-ref, kept and `fresh` semantics and the
`info/exclude` entries. Credentials never enter a context that executes
agent-writable hooks or configuration. Repository tasks therefore require `git`
in the agent image; official-base users add it with `FROM`.

**Git relay.** A controller relay mediates Git smart HTTP with a per-attempt
opaque credential. The attempt receives it only through `GIT_CONFIG_*`
environment entries that route HTTPS and scp-style remotes to the relay (ADR 0016).

`origin` keeps the real URL. The relay attaches controller-only, workspace-scoped
credentials per Git host and serves only the claim's repositories. Pushes follow
the native runtime: any ref the deployment credential may update. Grants end at
cleanup and on controller restart. Credential issuance and rotation, for example GitHub App tokens, stay
external, as files the controller rereads.

Rejected alternatives:
- Credentialed checkout in the controller or a sidecar: hooks and configuration
  the agent wrote would run with the credentials.
- Persisting the whole OpenCode data directory: it holds auth stores.
- Host bind mounts: the controller runs in a container.
- A sidecar container in the attempt's network namespace: it adds lifecycle
  without isolation gain.
- SSH agent forwarding.
- Hosting a Git service.
- Pre-cloned mirrors: they are preparation caches and belong to #5.

Refinement (#50, 2026-10-08). The helper is a separate digest-pinned image (Dockerfile
target `helper`, config `helper`); it also runs the volume init. Attempts get
the workdir volume only with the helper. The checkout route shares the Multica relay
listener. Checkout follows upstream isolated mode with a fresh clone and serves only
the claim's repositories.

Refinement (#52, 2026-10-08). Push follows the native runtime instead of an
`agent/<agent>/*` ref policy: native agents push with host credentials, so the
relay adds no ref checks. Checkouts set a per-host commit identity (default: agent
name) and upstream's Co-authored-by hook, following the setting polled every 10 s (#61).

Refinement (#55, 2026-10-08). Pull requests use the unchanged `gh`, as native.
Attempts resolve forge API names to loopback, and the helper forwards 443 to a
controller TLS relay. A per-process CA, name-constrained to those names and
trusted via `SSL_CERT_DIR`, signs its certificates. The relay swaps the Git
relay credential, sent as the `gh` token, for the workspace token.

## Consequences

Agents can work on repositories and continue sessions without credentials inside
the attempt. Retained volumes are part of the trust boundary. They are reused
only for the same workspace, agent and issue, and must not be returned to any
clean pool (ADR 0004). Each session pays for its first clone. Git credential
issuance and forge-side branch protection remain deployment-owned. The
relay bounds pushes only to the claim's repositories; credential scope and forge
branch protection bound their refs.

Implementation follows #47: 07.2 read path, 07.3 push, 07.4 resume and
retention, 07.5 pull requests. Kubernetes (#6) needs equivalent volumes and a
loopback-capable helper.
