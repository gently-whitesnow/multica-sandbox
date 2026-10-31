# ADR 0027: Token sources and explicit JWT identity profiles

Status: Accepted
Date: 2026-10-31

## Context

The controller's identity verifier assumed every token had Keycloak's `azp` and
`typ=Bearer`. An existing deployment uses OpenBao AppRole authentication and an
OIDC identity role instead. Copying that deployment's bootstrap credentials into
attempts or adding an application-specific authentication adapter would couple
the sandbox to its consumer. ADRs 0013 and 0022 describe the original issuer guard.

## Decision

Separate `TokenSource` from common JWT verification. Support OAuth2 client
credentials and OpenBao AppRole login followed by a configured identity-token
request. Keep all credentials and endpoints in trusted controller configuration;
re-resolve secret files or external bindings for each issuance. Never substitute
an OAuth ID token for an access token, accept an opaque token without validation,
or infer issuer endpoints from workload input.

Use explicit issuer profiles. `keycloak` verifies `azp` against the client's ID
and `typ=Bearer`; an omitted profile preserves the original contract. `jwt`
requires a configured audience and principal claim, plus an expected principal
in the trusted binding/resolver response. It has no Keycloak claim assumptions.
Both verify RS256/ES256 signature, exact issuer, nonempty subject, bounded lifetime,
expiry, issuance time and optional not-before. Configured audience is checked by
the controller as well as by the recipient. The original Keycloak profile without
an audience retains recipient-owned validation for configuration compatibility.
The `jwt` profile cannot use `agent_env`: its credential pair has no independently
trusted expected principal. This refines ADR 0022's claim-only binding.

OpenBao uses fixed same-origin login, identity-token and revoke-self endpoints.
Its temporary login token remains controller-only and is revoked after issuance,
including failure and cancellation. Failed cleanup denies token delivery. A named
AppRole policy authorizes only the identity signing role and self-revocation;
the signing role supplies audience, principal template and TTL. The sandbox has
no knowledge of consumer names, scopes or grant catalogues.

Identity delivery still checks the selected exact MCP URL before issuance.
The existing per-attempt opaque MCP relay credential, rotation, lease/revocation,
request cancellation and resource checks remain unchanged. A persistent logical
agent principal can span many isolated attempts without exposing its bootstrap
credential. This does not turn a service JWT into task attestation; deployment
MCP services and optional attempt authority own that distinction (ADR 0009).

Operator-configured `http_services` also use the identity source through the
existing MCP relay listener. Each service fixes one origin and explicit method/
path routes. The attempt receives an opaque connection URL/token under
`SANDBOX_HTTP_<NAME>_*`; source credentials/JWTs stay controller-side. This serves
CLI consumers that cannot use MCP without introducing a second proxy lifecycle.
The recipient owns service permissions; attempt end revokes the opaque grant and
cancels streams. No caller may select another origin or expand routes. HTTP query
parameters remain available within the approved route. A configured clean base
path is preserved. An explicit service `bearer_file` is a distinct static HTTP
credential source, like Git/inference credentials; it requires an `agent_ids`
allowlist and is never a fallback from JWT validation. Other agents receive no
service route or environment. Re-read it on renewal under a 30-second internal
lease; absent/unreadable credentials deny. Trusted constant `X-*` headers may
carry service metadata. No static service credential enters the attempt.

## Consequences

Existing Keycloak configurations remain compatible. Generic OpenBao deployments
need only issuer/source/binding configuration and a matching MCP verifier.
Changing token-source type does not require changing the OpenCode adapter.
Unit tests cover claim/source failures and cleanup; disposable Keycloak/MCP and
OpenBao fixtures exercise real issuance and rotation. This is an execution-boundary
integration, not a new identity issuer, generic proxy or authorization service.
