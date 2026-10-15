# ADR 0022: Inference connection and agent identity from Multica env

Status: Accepted
Date: 2026-10-15

## Context

Operators bound every workspace's gateway key (ADR 0012) and every agent's IAM
client (ADR 0013) in controller files or an external resolver. A deployment may
instead keep them where their owners already work: the workspace owner stores the
gateway URL and key in workspace env, the agent owner stores its IAM client in the
agent's `custom_env`.

Upstream Multica has agent `custom_env` in claims. Workspace env is proposed in
upstream PR 4521: `GET /api/workspaces/{id}/env` for owners and admins, audited
as `workspace_env_revealed`, and a claim whose agent `custom_env` merges workspace
env with agent values winning.

Gateway allow-lists, several issuers and per-binding subjects repeated what the
operator already names in bindings and added configuration without protecting a
distinct boundary.

## Decision

Both sources are opt-in and precede the existing configuration:

- Inference `workspace_env: true` reads `INFERENCE_BASE_URL` (the OpenAI-compatible
  base URL ending in `/v1`) and `INFERENCE_API_KEY` through the workspace env
  endpoint with the controller token, for each attempt and catalog refresh. The
  claim is not used: there an agent value overrides the workspace one, and model
  discovery has no claim.
- Identity `agent_env: true` reads `IDP_IAM_CLIENT_ID` and `IDP_IAM_CLIENT_SECRET`
  from the claim's agent `custom_env`. The attempt keeps that snapshot for renewal.

Both names present select the env connection. Neither name, or a server without
workspace env (404), falls back to bindings or the resolver. One name alone, a base
URL without `/v1` or an unreadable env deny the attempt, never another key.
`env_allow` cannot name these four variables, so they never reach attempts.

Allow-lists go: inference has no `gateways` list, only URL shape checks. Identity
has one `issuer`; bindings and resolver replies name only `client_id`. The controller
verifies the token's signature against the issuer's JWKS, issuer, `typ`, `azp` and
bounded lifetime, not its subject. `mcp` remains the exact URLs that receive the
agent's token.

## Consequences

The controller token user must be owner or admin of each workspace that uses
workspace env; every read adds an audit entry. Multica stores the values in plain
text and shows them to workspace admins and agent owners. Native runtimes of the
same agents receive them as process env, so the variables suit managed sandbox
runtimes only. With PR 4521, workspace `IDP_IAM_*` values reach every agent claim.

A workspace admin can send that workspace's key to any HTTPS gateway; the key is
the workspace's own. A client secret copied into another agent works there:
one-agent-per-client holds only for static bindings. Changed values apply to new
attempts; running ones keep their snapshot until they end.

## References

- [Upstream PR 4521: workspace global env](https://github.com/multica-ai/multica/pull/4521)
- [Inference module](../internal/inference/README.md), [identity module](../internal/identity/README.md)
