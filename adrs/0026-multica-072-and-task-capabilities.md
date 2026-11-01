# ADR 0026: Multica v0.7.2 and native task permissions

Status: Accepted; authorization decision revised 2026-11-01.
Date: 2026-10-31

## Context

The controller and mounted CLI use stable Multica v0.7.2, commit
`f05a525ac8e82f5e9169ad078e44f166fad6d5c8`. Its task token authenticates as
the runtime owner, with the task's agent attribution and workspace binding.
Workspace-role middleware sees the runtime owner's role: an admin-owned `mat_`
token has native admin authority. Agent attribution does not downgrade it.

The initial sandbox method/route allowlist blocked normal platform operations,
including an explicitly requested project creation and native workflows. The
owner chose Multica as the authority for agent API permissions and requested
removing the parallel sandbox policy rather than maintaining another role model.
The prior restriction remains recoverable from Git history.

## Decision

Forward every method and route under `/api/` to the configured Multica origin.
Remove both the method/route allowlist and the older administrative/credential
route denylist. Do not add per-role policy, a compatibility switch or another
approval mechanism. Project, workflow, skill and autopilot operations succeed or
fail according to the pinned native server's authorization and validation.

Keep credential translation and the execution boundary: the controller PAT and
raw upstream task token remain outside attempts. An opaque attempt grant fixes
the Multica origin and expires on cleanup/restart. The transport still refuses
non-API targets, traversal, redirects and malformed requests; it strips spoofed
identity headers. These rules protect credential routing, not platform roles.

There is no promise that an admin-owned task is a member or that credentials
returned by a native API are bounded by the relay grant's lifetime. Multica owns
those decisions. Use native membership/runtime ownership to change authority.
Container, network, MCP, HTTP service and inference boundaries are unchanged.

Preserve native chat/channel continuity and run-only autopilot instructions.
Autopilot configuration now follows the same native API permissions as other
platform operations. This does not change native channel attachment/file delivery
support or automatically enable existing paused schedules.

## Alternatives

- Extending the allowlist one operation at a time retains a second permission
  model and repeatedly blocks newly supported native features.
- Per-agent sandbox roles or a policy toggle add a separate authorization system
  the current deployment does not need.
- Rewriting identity headers cannot downgrade native task tokens: Multica
  resolves the actor and role from the token.

## Consequences

The current service-admin runtime gives attempts native admin authority within
Multica. The controller PAT is still not projected into containers. Native human
actor checks, workspace binding and role refusals remain effective. Allowed
upstream endpoints may return credentials or modify runtime/platform settings;
the sandbox does not filter those API responses or mutations.

The disposable pinned-server contract proves project creation with a repository,
workflow access, admin success/member refusal through the same relay, native
credential issuance refusal, cross-workspace refusal, agent-authored comments
and revoked-grant rejection. Production acceptance must use isolated fixture
work, without changing owner business projects or workflows.

## References

- [Claim delivery](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/internal/handler/daemon.go)
- [Pinned routes](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/server/cmd/server/router.go)
- `integration/task_authority_test.go`, `integration/native_claims_test.go`
