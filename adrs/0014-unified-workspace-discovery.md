# ADR 0014: One workspace discovery path

Status: Accepted
Date: 2026-10-05

## Context

The initial service selected one workspace; ADR 0008 added discovery while retaining
that launch path for compatibility. There are no deployed users requiring it.
Maintaining two execution paths complicates recovery and controller integration.

## Decision

Always discover the service identity's accessible Multica workspaces. Remove
`workspace` and `workspaces` configuration selectors and the separate sequential
service loop. One accessible workspace uses the same scheduler as many;
`concurrency` controls execution capacity independently of membership count.

Bind persisted controller ownership to server, daemon and Docker Engine. Reject
unknown identity fields and trailing documents rather than silently interpreting
an unsupported state format. No automatic state migration or compatibility shim.

## Consequences

One service launch, discovery, recovery and execution path remains. Token membership
continues to limit accessible workspaces. The standalone lifecycle probe can still
select a workspace for a disposable diagnostic; it is not a service mode.

Development configurations must omit the removed selectors. Stop older development
instances and confirm cleanup before using fresh state; never discard active
ownership state. ADR 0007's sequential service and ADR 0008's migration guidance
are superseded by this decision. Tests cover single-workspace service recovery
and multi-workspace discovery through the shared path.

## References

- [ADR 0008](0008-multi-workspace-controller.md)
- [Service entry point](../internal/service/run.go)
