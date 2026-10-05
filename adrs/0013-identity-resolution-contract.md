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
Keep issuer/token/JWKS endpoints and a bounded token lifetime in operator
configuration. Bindings carry optional OAuth issuance parameters; there is no
resource catalog. Separate delivery rules map exact selected MCP URLs to approved
issuer names. Missing rules, duplicate URLs and issuer mismatches deny delivery. The external resolver is trusted to assign principals to agents;
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
issuer, subject, client and bounded lifetime before exposing the bearer. IAM owns
audience, roles and groups; receiving MCP services validate audience and permissions.
Controller verification establishes identity, not recipient authorization.
Do not implement an issuer or assume that ID tokens and access tokens interchange.

## Consequences

The OpenCode controller adapter uses the issuance guard for approved MCP delivery
targets from trusted claims. Delivery rules never add connections to the set selected
by Multica. Separate attempt grants, atomic projection, renewal and cancellation
follow ADRs 0009 and 0011. A reference alone is not workload attestation.
Inference endpoint/model bindings are separate; ADR 0012 reuses this credential
contract for inference-specific issuance.

The existing identity/MCP fixture now exercises both resolution paths against
real Keycloak. Local tests cover protocol/tenant failures, credential changes and
JWT verification. Issuers with different access-token conventions need a separate
adapter; accepting arbitrary JWTs is not a compatibility feature.

## References

- [Module contract and code walkthrough](../internal/identity/README.md)
- [OAuth2 client credentials](https://pkg.go.dev/golang.org/x/oauth2/clientcredentials)
- [go-oidc verification](https://pkg.go.dev/github.com/coreos/go-oidc/v3/oidc)
