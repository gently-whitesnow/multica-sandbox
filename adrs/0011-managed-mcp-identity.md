# ADR 0011: Managed MCP identity delivery

Status: Accepted; explicit selection refined by ADR 0030.
Date: 2026-10-04

## Context

Users select agent MCP connections in Multica. Long tasks need token rotation
without exposing IAM bootstrap credentials or maintaining another tool catalog.

## Decision

Use effective MCP configuration delivered in a trusted Multica claim. An explicit
`Authorization: Bearer {iam:access_token}` header selects the agent identity.
Ordinary headers and anonymous connections do not implicitly receive it. ADR 0030
removes the earlier deployment lists of exact identity and anonymous recipients.

Resolve credentials from native Agent Environment, an explicit static binding or
an authenticated external resolver. The issuer and token-source endpoints belong
to deployment configuration. Verify source/client identity and signed JWT claims
before delivery. IAM owns audiences, roles and groups; MCP services filter
`tools/list` and authorize every `tools/call` and concrete resource.

The controller keeps source credentials and JWTs. Its relay grants the attempt
an opaque per-attempt credential and forwards only its selected connection with
the current upstream authorization. Reissue tokens while Multica confirms the
task remains running. Revoke attempt grants and cancel streams on cleanup.
Stopping renewal alone does not invalidate an upstream unexpired JWT.

An optional external authority may enforce attempt fingerprint leases (ADR 0009).
Logical-agent identity is not task attestation; deployments must not assume every
issuance produces a unique JWT. OpenBao subject proof and generic JWT validation
are specified in ADR 0027 and ADR 0030.

All connections follow the deployment transport policy. When a public proxy is
configured, identity connections use it too; selecting identity grants no direct
network bypass. Credentials and caller query cannot redirect the selected target.
Keep controller configuration outside the attempt and use mediated network access.

## Consequences

New connections and credentials are managed through existing Multica forms.
No per-agent UUID or MCP recipient list is required in the ENV deployment mode.
The same runtime supports Keycloak and OpenBao through existing token sources.
Inference uses its own workspace-key relay (ADR 0012), not the agent identity.

Maintained disposable tests exercise real Keycloak/OpenBao issuance and native
OpenCode calls across expiries, issuer/resolver outages, cancellation, workspace
separation, ended-grant rejection and retained connection updates. Full deployment
acceptance also checks the actual network policy and native claim delivery.
