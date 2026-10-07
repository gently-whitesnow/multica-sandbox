# ADR 0011: Managed MCP identity delivery

Status: Accepted
Date: 2026-10-04

## Context

Users select each agent's MCP connections in Multica. Long tasks need token
rotation without exposing OIDC client secrets or maintaining a second tool catalog.

## Decision

Use the effective MCP configuration delivered by Multica. Do not add connections
from controller policy. Operators configure identity bindings by Multica server,
workspace ID and agent ID; client ID is a resolved IAM attribute, not a selector
supplied by the workload. Verify the issued principal against the binding.

Define an identity resolver with static configuration and an authenticated external
service implementation. Static bindings reference controller-only secret files;
external resolution can obtain credentials from deployment-owned secret storage.
Keep client secrets outside the workload and out of logs and persisted task state.

Define MCP identity delivery rules, initially static, with an interface for future
external resolution. Match exact approved server URLs to issuer names, not
user-selected connection names. IAM bindings carry optional OAuth issuance
parameters. Do not require placeholder headers or per-tool scopes in Multica.
IAM owns token audiences, roles and groups; MCP validates its intended audience
and permissions. The controller has no corporate resource catalog. Never send corporate tokens to arbitrary URLs.
Reject conflicting supplied authorization rather than silently overwriting it.

IAM supplies identity roles/groups. MCP services filter `tools/list` and authorize
every `tools/call`, including its concrete resource. The controller does not own
tool permissions. Preserve attempt authorization and revocation from ADR 0009;
stopping renewal alone does not invalidate an unexpired token.

The controller obtains new access tokens while an attempt remains authorized.
Agent adapters project them into a per-attempt token store. For OpenCode, use its
native MCP OAuth store with access tokens, expiry and matching server URLs only;
do not deliver client secrets or refresh tokens. Update atomically and remove
the projection on cleanup. Runtime environment variables/static headers are not
a refresh mechanism. No additional renewal process is required.

## Consequences

The experimental persistent controller registers OpenCode (verified releases in `internal/opencode`) and consumes
its effective remote `mcpServers` claim selection. It rejects unsupported broker
connections, local commands, supplied headers/OAuth credentials and mismatched
agent identity. Arbitrary claim environment and host paths are excluded.

The controller uses `docker exec` stdin to atomically replace the store in each
container's tmpfs; no host token directory or credential mount is needed. It renews
inside the running attempt, checks Multica status every second, and stops on any
issuer, resolver, authority or projection failure. A separate external attempt
integration registers fingerprints before projection and enforces a 15-second lease;
cleanup revokes all fingerprints for that attempt. See ADR 0009.

The Docker path creates an internal network per attempt and attaches only operator
approved MCP/gateway peers from a validated template. Issuers, resolvers and Multica
stay outside those networks. OpenCode uses its native MCP transport and OAuth store.
External deployment policy remains required. The global config directory
is read-only to avoid startup package installation; images may provide their own tools.

Maintained tests run two native OpenCode tasks with real Keycloak JWTs across
multiple expiries, using a credential-free deterministic model fixture. They assert
successful MCP calls, distinct token versions, workspace separation,
issuer/resolver outages, cancellation and denial of still-unexpired ended tokens.
The upstream fixture also exercises a real claim through the Compose controller.
Inference does not use this identity; it goes through the workspace-key relay from ADR 0012.
The first adapter maps native events/usage/session IDs and safe repository
references as recorded in its README. Repository materialization and native
session resume remain unsupported; this is not production adapter certification.

## References

- [OpenCode native MCP auth store](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/opencode/src/mcp/auth.ts)
- [OpenCode OAuth provider](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/opencode/src/mcp/oauth-provider.ts)
- [MCP authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
