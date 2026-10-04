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

Define an MCP authorization policy resolver, initially static, with an interface
for future external resolution. Match approved server URLs, not user-selected
connection names. Each rule selects trusted token issuance parameters for that
resource. Do not require placeholder headers or per-tool scopes in Multica.
Scopes/resource parameters remain optional issuer-specific settings; tokens must
be intended for their recipient. Never send corporate tokens to arbitrary URLs.
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

A disposable experiment with OpenCode 1.18.34 confirmed that one running process
uses a replacement access token from `mcp-auth.json`; changing a static header
configuration retained the old token. It used synthetic bearer tokens and an
isolated MCP fixture, not a long-running real-Keycloak task.

The native store is an OpenCode-specific internal format, not a portable runtime
API. Its projection, version compatibility, real JWT renewal, issuer outages,
tenant isolation and cancellation remain implementation and conformance work.
Controller identity resolution and rotation are not implemented by this ADR.

## References

- [OpenCode native MCP auth store](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/opencode/src/mcp/auth.ts)
- [OpenCode OAuth provider](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/opencode/src/mcp/oauth-provider.ts)
- [MCP authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
