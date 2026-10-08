# Repository checkout and Git relay

Agents run the unchanged upstream `multica repo checkout <url> [--ref] [--fresh]`
inside their attempt, then commit and `git push` as under the native runtime (ADR 0015).
The controller serves upstream's daemon `/repo/checkout` contract and mediates Git smart
HTTP. No Git host or Multica credential enters the attempt. With `sessions`, follow-up
tasks on an issue reuse its workdir and checkouts ([OpenCode README](../opencode/README.md)).
With the forge relay, the unchanged `gh` opens pull requests.

## Configuration

Add these to `opencode` (`deploy/opencode.example.json`). They require `multica_relay`:

- `helper`: the digest-pinned image from `docker build --target helper .`, preloaded
  like the agent image. It is image-mounted at `/opt/multica-sandbox/helper` and kept
  off `PATH`.
  - `sandbox-helper init` prepares each attempt's owned volume as uid 65532 with mode
    0700. It runs with no network, a read-only rootfs and only `CAP_CHOWN`.
  - The volume is the agent working directory `/workspace/work`. Projected
    configuration, auth stores and `HOME` stay in the `/workspace` tmpfs.
  - `sandbox-helper forward` serves `127.0.0.1:19514` (`MULTICA_DAEMON_PORT`) as the
    attempt user, towards the Multica relay listener.
- `git_relay` (`listen`, `url`): the agent-facing Git relay. Give the controller the
  `url` alias on the template network, like the other relays.
- `git_file`: a controller-only version-1 file (`deploy/git.example.json`). Each entry
  binds a `workspace_id` and a claim URL `host` to an `upstream` origin (default
  `https://<host>`; `http` needs `allow_http`).
  - With `password_file`, the relay sends basic authentication with `username`. The
    file is reread per request, so an external issuer can rotate it, for example a
    GitHub App installation token with `x-access-token`.
  - Without a password file, the host is fetched anonymously.
  - `api` (with a password file) is the forge API origin for `gh`, for example
    `https://api.github.com` or an Enterprise Server origin.
  - `commit_name` and `commit_email` set the commit identity of the host's checkouts,
    for example a GitHub App bot. They default to the agent name and
    `agent@multica-sandbox.invalid`.

The agent image must contain `git`; startup checks it. Official-base users add it with
`FROM`, as `examples/agent-image/debian.Dockerfile` already does.

## Behavior

Attempts receive `GIT_CONFIG_*` entries:
- `http.<relay>/.extraHeader` carries a per-attempt `msg_` credential;
- `url.<relay>/<host>/.insteadOf` maps `https://<host>/` and `git@<host>:`.

`origin` keeps the real URL. The relay serves only smart HTTP fetch and push
(`git-upload-pack`, `git-receive-pack`) for the claim's repositories on bound hosts.
It replaces the credential and refuses redirects. Packs may reach 2 GiB, and the
upstream gets 5 minutes to answer. Grants end at cleanup and on restart.

`forge_relay` (`listen`, an `https` `url` on the controller alias) serves `gh`, as native
`gh` uses host credentials:
- Attempts resolve each `api` name (`api.github.com` for `github.com`, otherwise the
  host, `/api/` only) to loopback, where the helper forwards port 443 to the relay.
- The relay terminates TLS with certificates from a per-process CA. The CA is
  name-constrained to those names and trusted through `SSL_CERT_DIR`
  (`/workspace/certs`).
- `GH_TOKEN` and `GH_ENTERPRISE_TOKEN` carry the attempt's `msg_` credential, and
  `GH_HOST` names a single bound host. The relay sends `token <password>` upstream.
- API scope is the deployment credential's. Images add `gh` themselves, as the Debian
  example does.

Pushes follow the native runtime, where agents push with the host's credentials: any
ref the deployment credential may update. Scope that credential and protect branches
on the forge.

`/repo/checkout` authorizes requests in this order, with upstream's messages:
1. the attempt's Multica relay credential;
2. the claim's workspace and task;
3. a workdir inside `/workspace/work`, resolved again in the container;
4. an exact claim URL;
5. a ref without option or range syntax.

It then runs the embedded `checkout.sh` with `docker exec` as uid 65532, with the
attempt's Git environment. The script follows upstream isolated mode (`repocache` at
b4ca5b4) with a fresh clone instead of the bare cache:
- **Path and branch:** the checkout goes to `<workdir>/<repo>`, on branch
  `agent/<agent>/<task12>` from the requested or claim ref, or else the default branch.
  The branch name gets a `-<unix>` suffix on collision.
- **Kept checkouts:** an existing checkout is kept when it is on the task branch
  (`task_branch`) or holds work (`local_work`); only its remote refs are fetched.
- **`--fresh`:** resets and cleans, then prunes `agent/*` branches without unpushed
  commits.
- **Agent files:** `info/exclude` lists the upstream agent files.
- **Busy:** a concurrent checkout waits 10 s, then returns 503 `repo-busy`.
- **Commits:** every checkout writes the host's commit identity to the repository
  config, standing in for the native host's global identity. It also reconciles
  upstream's `prepare-commit-msg` hook, which adds
  `Co-authored-by: multica-agent <github@multica.ai>`. The hook is installed while the
  workspace's `github_enabled` and `co_authored_by_enabled` are on (default) and
  removed otherwise; the setting is read once per attempt.

Differences from upstream:
- only the claim's repositories, not the whole workspace registry;
- every retained workdir pays for its own clone;
- the identity is repository config of the checkout, not a global one, so clones made
  without `multica repo checkout` have none;
- claims with a non-HTTPS repository URL are rejected, as before; scp-style remotes
  of bound hosts are only rewritten.
