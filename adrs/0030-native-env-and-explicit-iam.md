# ADR 0030: Native environment and explicit IAM headers

Status: Accepted.
Date: 2026-11-01

## Context

Agent Environment promised process injection but required a second controller
allowlist. Identity credentials and MCP recipients also duplicated native Multica
configuration in deployment files. The owner requested the existing idp-sandbox
workflow for both Keycloak and OpenBao without modifying Multica.

## Decision

Agent Environment is the source of ordinary process environment. Remove
`opencode.env_allow`; pass valid names and values without a per-name operator
list. Controller/image-owned names remain reserved. Values consumed as IAM
credentials or MCP header secrets stay controller-only. Preserve redaction.

Use `IAM_CLIENT_ID` and `IAM_CLIENT_SECRET` with `agent_env: true`. For Keycloak
these identify its service-account client; for OpenBao they are the AppRole
role ID and secret ID. Remove the previous IDP-prefixed names without aliases.
The agent owner selects the external identity by assigning credentials through
native Multica permissions. Copying credentials copies that external identity;
this mode does not attest a unique relationship to a Multica agent UUID.

OpenBao retains `auth.entity_id` from successful login. After signature, issuer,
audience and lifetime verification, require JWT `sub` to equal that entity.
Principal claims come from the verified JWT, not another environment assertion.
Keycloak retains its client/authorized-party checks. Static and external sources
remain explicit capabilities, with their existing expected-principal checks.

An MCP receives identity only when its saved Authorization header is exactly
`Bearer {iam:access_token}`. Parse this literal before ENV substitution; ENV cannot
manufacture IAM selection. Unknown or mixed IAM templates fail safely. Additional
ordinary headers may coexist. Static headers and `{env:NAME}` retain their own
credential modes; IAM bootstrap credentials cannot be used as header variables.
Remove the identity `mcp` and `mcp_anonymous` URL lists and implicit URL matching.

Reuse token renewal, opaque attempt grants, stream cancellation and cleanup.
Neither JWTs nor source credentials enter attempt config, process ENV or logs.
All selected MCP connections use the configured public proxy, including IAM;
choosing identity cannot grant direct network access. Without a configured proxy,
an operator intentionally owns direct routing, as in isolated native fixtures.
Deployment network rules may admit an internal service by host/port independently
of agent identity or selected MCP paths. No redirect or direct proxy fallback.

## Consequences

The existing Multica MCP header form selects identity and displays that choice.
New agents need only Environment credentials and MCP assignment, without a
controller UUID binding or per-endpoint recipient rollout. Changes apply on the
next attempt; active attempts retain their claim snapshot. No new UI, token
broker, profile database or queue is introduced.

The Ecosystem deployment keeps its single-workspace inference and existing
Overtime, Telegram and Git integrations. Their migration is separate work.
Trusted issuer/source endpoints, audience, controller credentials, network policy
and immutable images remain deployment configuration. Migration updates both the
workspace MCP library and any agent override; verify native readback before
removing obsolete controller credential projections.

### Verification

Cover Keycloak and real disposable OpenBao issuance, missing or mismatched entity,
wrong issuer/audience/lifetime, failed cleanup, partial credentials, native claim
headers, retained-session updates, ordinary ENV projection and credential absence.
Exercise IAM through the proxy and preserve private/metadata denial except the
declared service route. Native attempts must reject grants after task cleanup.
