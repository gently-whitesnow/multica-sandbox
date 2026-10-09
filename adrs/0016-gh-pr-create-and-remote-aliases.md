# ADR 0016: gh pr create through Git remote aliases

Status: Accepted
Date: 2026-10-09

## Context

ADR 0015 (#55) relays `gh` to forge APIs, verified only with `gh api` REST. Agents
open pull requests natively with `gh pr create`, and upstream's brief asks for
`gh pr create --base <branch>`. A probe with gh 2.46 (the Debian example image)
established these facts:
- `gh pr create` posts three GraphQL operations: `RepositoryInfo`,
  `PullRequestForBranch` and the `PullRequestCreate` mutation. It sends them to
  `https://api.github.com/graphql` for github.com and to `https://<host>/api/graphql`
  for Enterprise Server, with `token <GH_TOKEN|GH_ENTERPRISE_TOKEN>`. It sends no REST
  calls without reviewers, labels or similar options.
- gh resolves the base repository from `git remote -v`. That output shows
  `url.<base>.insteadOf` targets. The Git relay layout `<relay>/<host>/<path>`
  therefore hid the forge host, and gh found no remote matching `GH_HOST`.
- gh takes the host from the remote URL without its port, so any URL on the forge
  host resolves.
- Without hosts in its configuration, gh resolves Enterprise Server remotes only
  through `GH_HOST`; github.com remotes resolve without it.

## Decision

**Remote aliases.** Each claim host gets the alias `http://<host>/`:
- `url.http://<host>/.insteadOf` rewrites `https://<host>/` and `git@<host>:`;
- `http.http://<host>/.proxy` sends the alias to the Git relay as an HTTP proxy;
- `http.http://<host>/.extraHeader` carries the opaque credential.

`origin` keeps the real URL. `git remote -v` shows a URL on the forge host, so the
unchanged `gh` resolves the repository. Git does not resolve the alias name; it
sends absolute-form requests to the relay. The relay accepts only absolute-form
`http` targets without user information. It selects the binding by the target host
and keeps every ADR 0015 check: grant, workspace binding, claim repositories, smart
HTTP services and revocation. Origin-form requests and `CONNECT` are refused. The
former path layout is removed.

**GraphQL.** The forge relay forwards `/graphql` (`api.github.com`) and
`/api/graphql` (Enterprise Server) unchanged to the configured `api` origin, like
REST. It keeps the TLS name, Host header, workspace and grant checks. The fixture
forge serves the three operations, and the fixture agent runs the unchanged
`gh pr create`.

**API scope.** Scope stays the deployment credential's, as for native `gh`
(user decision 2026-10-08, #55). The relay inspects neither GraphQL operations nor
REST paths. Forge branch protection and credential scope are deployment-owned bounds
for pushes and `gh`. Narrowing options await a user decision in #62.

Rejected alternatives:
- `GH_REPO`: it is single-valued and overrides the working directory's repository.
- Loopback name resolution for Git hosts with a helper forward: more attempt
  configuration and Docker policy, without isolation gain over the proxy.
- Git over the TLS forge relay: Git would depend on `forge_relay` and on its own CA
  trust configuration.
- A projected gh `hosts.yml` for workspaces with several API hosts: deferred until
  such deployments exist. Until then, Enterprise Server needs `GH_HOST` or `--repo`
  there.

## Consequences

The unchanged `gh pr create` works against the fixture for github.com and Enterprise
Server paths. Behavior against real github.com and Enterprise Server stays unverified,
since no forge credentials are available to the tests. The alias is plain HTTP on the
attempt network, like the former relay URL. Tools that read `git remote -v` see
`http://` URLs, while `origin` itself keeps the HTTPS URL. Kubernetes (#6) needs no
name resolution for Git hosts.
