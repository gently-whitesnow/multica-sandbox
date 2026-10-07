# ADR 0014: Multica API credential relay

Status: Accepted
Date: 2026-10-06

## Context

The upstream Multica daemon gives an agent only the issue ID plus instructions to
use the `multica` CLI. The CLI authenticates with the claim's task-scoped
`auth_token` (`mat_`), delivered as `MULTICA_TOKEN` with `MULTICA_SERVER_URL`.
A `mat_` token acts as the originator user and agent in the workspace. Multica
deletes it on completion/failure and expires it after 24 hours. ADR 0003 forbids
Multica tokens inside workloads, so a sandboxed agent cannot read its task or
reply. Contract inspection at `b4ca5b4a23e68b26292a680dca7689a952bb1cd5` also
found that a `mat_` token can call `POST /api/tokens` to mint a personal access
token that outlives the task.

## Decision

Keep the agent working like upstream: the unmodified upstream CLI and
upstream-equivalent instructions. Translate the credential outside the sandbox
with the same mechanism as ADR 0012's inference relay. One shared module,
`internal/relay`, is a reverse proxy, not a redirect. Each grant fixes one trusted
origin, credential and optional attribution headers; credentials carry a
relay-specific prefix. It validates an active grant before forwarding and
replaces `Authorization`.
It removes cookies, forwarding and identity headers. It refuses upgrades,
absolute-form targets, encoded or traversing paths and upstream redirects. It
bounds bodies and in-flight requests, cancels in-flight requests on revocation
and returns fixed error bodies for its own failures without logging. A policy can
reserve header prefixes and withhold upstream error bodies; the Multica relay
passes API errors through. Caller fields never select the upstream or the
credential.

The Multica relay is explicitly enabled per controller (`multica_relay`). Its
grants use the controller's configured Multica origin. The claim's `mat_` token
is decoded into a redacted type that formats and marshals as nothing. It stays in
controller memory, never in logs, results, the terminal-report outbox or JSON.
A claim without a `mat_` token is rejected before the workload starts, matching
upstream. Each attempt receives an opaque credential `mat_relay_<64 hex>`. The
upstream CLI requires the `mat_` prefix. A grant forwards only while the attempt
is active. Cleanup revokes it before the agent process is joined, so completion,
cancellation, renewal failure and controller recovery all end access. A
controller restart loses every grant and fails closed.

The relay denies credential, session, daemon and account surfaces:
`/api/daemon`, `/api/tokens`, `/api/cli-token`, `/api/auth`, cloud billing,
runtime and subscription routes, `/api/integrations`, `/api/plugin-bridge`,
invitations and share links, plus every non-`/api/` path including `/ws`.
Re-check this list on each upstream upgrade.

The controller is an approved peer on the execution template network. Attempts
reach the relay by its alias; Multica itself stays off attempt networks. The
OpenCode adapter delivers `MULTICA_SERVER_URL`, `MULTICA_TOKEN`, workspace, agent
and task IDs through the exec client environment, not argv or container
configuration. It projects the upstream issue prompt and an `AGENTS.md` brief
mirrored from `daemon/prompt.go` and `execenv` at the pinned revision. With the
relay, local in-container tools (`bash`, file tools) are allowed; network tools stay
denied. Multica still creates a comment from `output` when the agent posted none.
The CLI follows the controller's pinned Multica revision, not the image. Refinement
(#41, 2026-10-07): the controller mounts a digest-pinned CLI artifact (`multica_cli`,
`deploy/multica-cli.Dockerfile`) read-only at `/opt/multica-sandbox/multica`, first
on `PATH` (ADR 0002); the relay requires it. Images need no `multica`, and a copy in
the image cannot shadow it.

## Consequences

This is a deliberate exception to ADR 0003. Inside the sandbox, the opaque
credential has the authority of the upstream daemon's agent: the originator user
and agent across the workspace for the attempt's lifetime, minus the denied
surfaces. It cannot outlive the attempt or be exchanged for a Multica token.
Abuse of granted operations remains possible, as with upstream. The relay enlarges
controller responsibility and adds a listener reachable from attempts. Production
egress policy and TLS on the attempt network remain deployment-owned. Only
issue tasks (assignment or comment trigger) use upstream instructions; chat and
other task kinds keep the bounded legacy prompt.

Unit tests cover translation, isolation between attempts, ended/forged/upstream
credentials, redirects, error withholding, path policy, absolute-form targets, body
limits and in-flight revocation. A containers test keeps exec env out of container
configuration. The pinned integration fixture runs a real claim through the
Compose controller and the mounted upstream CLI on the Debian and official images,
neither containing `multica`. It checks the issue read, one agent comment, the CLI
first on `PATH`, no `mat_` token in agent env, files, controller logs or transcript,
the `/api/tokens` denial, Multica unreachable from the attempt and ended-credential
denial. Containers tests cover read-only mounts, shadowing and artifact rejection.

## References

- [Daemon environment and token handling](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/daemon/daemon.go)
- [Per-turn prompt](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/daemon/prompt.go)
- [Task-token authentication](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/middleware/auth.go)
- [Personal access token handler](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/personal_access_token.go)
