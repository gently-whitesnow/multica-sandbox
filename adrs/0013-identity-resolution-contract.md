# ADR 0013: Identity resolution contract

Status: Accepted
Date: 2026-10-04

## Context

ADR 0011 needs one resolution interface for static installations and external
secret infrastructure. The resolver must not determine arbitrary credential-bearing
network destinations or mix one workspace's identity with another's task.

## Decision

Resolve an exact trusted reference: Multica origin, workspace UUID and agent UUID.
Use either static bindings with absolute secret-file references or an authenticated
HTTP resolver with a version-1 JSON contract. Do not fall back between sources.
An external response must echo the reference and name a locally approved issuer.
Keep issuer/token/JWKS endpoints and resource audience/lifetime settings in operator
configuration. The external resolver is trusted to assign principals to agents;
MCP and inference remain responsible for authorizing attempts and resources.

Resolve credentials afresh on issuance, allowing secret and binding changes without
persisting resolved secrets. Static principal/client bindings cannot be shared by
different agents. External services enforce the equivalent consistency rule.
Reject malformed/oversized responses, redirects and unapproved endpoints. Disable
proxy environment variables and require HTTPS outside explicitly insecure fixtures.
Use bounded requests and sanitized errors; make credential serialization explicit
and unavailable through ordinary JSON or formatting.

Reuse OAuth2 client credentials and go-oidc verification. The first issuer adapter
supports Keycloak RS256 access tokens with typ=Bearer and azp. Verify the expected
issuer, subject, client, recipient and bounded lifetime before exposing the bearer.
Do not implement an issuer or assume that ID tokens and access tokens interchange.

## Consequences

The module is reusable by the controller but is not yet wired into task claims.
Trusted input provenance, attempt grants, delivery, renewal and cancellation remain
#19/#22 integration work. A reference alone is not workload attestation.

The existing identity/MCP fixture now exercises both resolution paths against
real Keycloak. Local tests cover protocol/tenant failures, credential changes and
JWT verification. Issuers with different access-token conventions need a separate
adapter; accepting arbitrary JWTs is not a compatibility feature.

## References

- [Module contract and code walkthrough](../internal/identity/README.md)
- [OAuth2 client credentials](https://pkg.go.dev/golang.org/x/oauth2/clientcredentials)
- [go-oidc verification](https://pkg.go.dev/github.com/coreos/go-oidc/v3/oidc)
