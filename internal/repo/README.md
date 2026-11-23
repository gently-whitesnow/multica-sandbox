# Repository checkout and Git relay

Agents run unchanged upstream `multica repo checkout <url> [--ref] [--fresh]` inside
attempts, then commit and `git push` as under the native runtime (ADR 0015). The
controller serves upstream's `/repo/checkout` contract and mediates Git smart HTTP; no
Git host or Multica credential enters the attempt. With `sessions`, follow-up tasks on
an issue or chat reuse its workdir and checkouts ([OpenCode README](../opencode/README.md)).
With `forge_relay`, unchanged `gh`/`glab` opens pull/merge requests.

## Configuration

Add to `opencode` (`deploy/opencode.example.json`); all require `multica_relay`:

- `helper`: digest-pinned preloaded image (`docker build --target helper .`), mounted
  at `/opt/multica-sandbox/helper` off `PATH`.
  - `sandbox-helper init` prepares each attempt's volume as uid 65532 mode 0700 (no
    network, read-only rootfs, only `CAP_CHOWN`). The volume holds `/workspace/work`,
    `HOME` caches and `TMPDIR` (ADR 0015); projected config, XDG config/data/state and
    auth stores stay in `/workspace` tmpfs.
  - `sandbox-helper forward` serves `127.0.0.1:19514` (`MULTICA_DAEMON_PORT`) as the
    attempt user towards the Multica relay listener.
- `git_relay` (`listen`, `http` `url`): Git smart-HTTP proxy; add its alias to the
  controller on the template network.
- `git_file`: controller-only version-1 file (`deploy/git.example.json`) binding
  `workspace_id` and claim `host` to `upstream` (default `https://<host>`; `http` needs
  `allow_http`).
  - `password_file` + `username`: basic auth, reread per request for external rotation
    (e.g. GitHub App installation token with `x-access-token`). Omit for anonymous fetch.
  - `api` (with `password_file`): forge API origin (e.g. `https://api.github.com`,
    Enterprise Server or GitLab). `forge` selects `github` (`gh`, default) or `gitlab`
    (`glab`; sends `PRIVATE-TOKEN`, or bearer with `api_auth: "bearer"`).
  - `commit_name` / `commit_email`: checkout commit identity (default: agent name and
    `agent@multica-sandbox.invalid`).

The agent image must include `git` (verified at startup; see `examples/agent-image/debian.Dockerfile`).
With `mirrors`, new checkouts borrow objects from read-only per-workspace mirrors (ADR 0035).

## Behavior

Attempts receive `GIT_CONFIG_*` entries per bound workspace host around `http://<host>/` (ADR 0016):
- `url.http://<host>/.insteadOf`: maps `https://<host>/`, `git@<host>:`, `ssh://git@<host>/`
  and any alternate claim URL prefix (`ssh://user@<host>:port/`, `user@<host>:`, `git://<host>/`).
- `http.http://<host>/.proxy`: routes the alias to `git_relay.url`.
- `http.http://<host>/.extraHeader`: carries a per-attempt `msg_` credential.

`origin` keeps the real URL and `git remote -v` shows the forge host alias, so `gh`
resolves repositories natively. The relay accepts only proxy requests for smart HTTP
fetch/push (`git-upload-pack`, `git-receive-pack`) to bound hosts, swaps credentials,
refuses redirects, allows packs up to 2 GiB with a 5-minute upstream timeout, and drops
grants on cleanup or restart.

`forge_relay` (`listen`, `https` `url` on controller alias) serves `gh`/`glab`:
- Attempts resolve each `api` hostname (`api.github.com` for `github.com`, else host
  `/api/` only) to loopback, forwarded by the helper on port 443 to the relay.
- `NO_PROXY`/`no_proxy` preserve image bypasses and add API names on port 443 so Git
  HTTP aliases stay on the Git relay.
- The relay terminates TLS using a per-process CA name-constrained to those hosts,
  trusted via `SSL_CERT_DIR` (`/workspace/certs:/etc/ssl/certs`), and verifies Host
  matches TLS SNI.
- GitHub: `GH_TOKEN`/`GH_ENTERPRISE_TOKEN` carry `msg_`, `GH_HOST` names a single bound
  host, and the relay sends `token <password>` upstream. Forwards GraphQL (`/graphql` or
  `/api/graphql`) and REST (`/api/v3/`) unchanged; passes `304` and absolute `https`
  redirects for log/artifact downloads (followed without relay credentials).
- GitLab: `GITLAB_TOKEN` carries `msg_`, `GITLAB_HOST` names a single bound host, and
  `glab` prompts/update checks/telemetry are disabled. Admits encoded slashes
  (`group%2Fproject`) on `/api/v4/` and `/api/graphql` after verifying a clean decoded
  path. Refuses token rotation (`.../personal_access_tokens/<id|self>/rotate`,
  `.../access_tokens/<id|self>/rotate`) with 403 regardless of method, case, trailing
  slash or suffix.
- Images provide `gh` or `glab` themselves (`examples/agent-image/debian.Dockerfile` includes `gh`).

Deployment-owned bounds:
- **Credential scope:** pushes and `gh`/`glab` have whatever authority the host
  credential holds. Use a scoped GitHub App token or fine-grained token (`contents: write`,
  `pull_requests: write`, no admin) or a GitLab group/project token (Developer, `api`).
- **Branch protection:** protect default and release branches on the forge.

Limits: beyond GitLab token rotation, the relay does not inspect REST paths or GraphQL
operations (#62). With multiple bound API hosts, `GH_HOST` is unset (pass `--repo` or
`GH_HOST` for Enterprise Server); behavior beyond the fixture is unverified.

## Checkout contract

`/repo/checkout` authorizes requests in order with upstream messages:
1. attempt's Multica relay credential;
2. claim's workspace and task;
3. workdir inside `/workspace/work` (re-verified inside the container);
4. exact claim URL;
5. ref without option or range syntax.

It runs embedded `checkout.sh` via `docker exec` as uid 65532 with the attempt's Git
environment, matching upstream isolated mode (`repocache` v0.5.2) with fresh clones:
- **Path/branch:** `<workdir>/<repo>` on `agent/<agent>/<task12>` from requested/claim
  ref or default branch (`-<unix>` suffix on collision).
- **Kept checkouts:** kept when on `task_branch` or holding `local_work` (remote refs fetched).
- **`--fresh`:** resets/cleans and prunes `agent/*` branches without unpushed commits.
- **Agent files:** excluded via `.git/info/exclude`.
- **Busy:** concurrent checkout waits 10 s, then returns 503 `repo-busy`.
- **Commits:** global Git config (`/workspace/config/git/config`) holds the common host
  identity (or agent default); each checkout writes its host's identity locally and
  reconciles `prepare-commit-msg` to add `Co-authored-by: multica-agent <github@multica.ai>`
  while `github_enabled` and `co_authored_by_enabled` are on (default).

Co-author setting propagation:
- Controller writes `1`/`0` to `/workspace/.multica_co_authored_by` (missing keeps trailer).
- Polls at attempt start, before checkout and every 10 s (replacing upstream websocket
  hints; failed reads keep the last value), reconciling hooks up to two levels below
  workdir without touching foreign hooks. Retained checkouts converge on next start.
- As natively, the agent can edit the hook/file; the trailer is attribution, not a control.

Differences from upstream:
- Checkouts outside the claim admit workspace repositories after a refresh (default
  branch), not other tasks' project repositories (`taskRepoURLs`).
- Each retained workdir holds its own clone.
- Setting changes propagate within 10 s; failed reads keep the previous value.
- Claim URLs accept https, http, ssh, git and scp-like forms, fetched over HTTPS from
  bound `upstream` (SSH/git ports ignored; passwords, queries or fragments reject the claim).
