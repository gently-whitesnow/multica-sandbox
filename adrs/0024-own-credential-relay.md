# ADR 0024: Keep the credential relay in-house

Status: Accepted
Date: 2026-10-30

## Context

ADR 0004 requires a recorded unmet requirement before building what a maintained
component could provide, and forbids a generic credential proxy. `internal/relay`
(grants and reverse proxy, about 380 lines) and its specializations already serve
Multica API (ADR 0014), inference (ADR 0012), Git and forge (ADR 0015, 0016) and MCP
(ADR 0023) without that record. Swapping an in-sandbox placeholder for a real
credential at an outside proxy is now a common pattern, so the question is whether a
product can replace the code, not whether the design is sound.

The relay must do three things together:

1. **Attempt-bound grants.** The controller issues an opaque per-attempt credential at
   claim and revokes it at cleanup, before the agent process is joined. A controller
   restart loses every grant and fails closed. No grant state is persisted.
2. **Revocation ends work in flight.** Revoking a grant cancels open requests and
   streams (SSE, streamed completions, MCP streamable HTTP), not only new requests.
3. **A closed policy per upstream.** One fixed origin and credential per grant;
   Multica API path deny-list; inference on one route with attribution, reserved
   `x-litellm-*` headers and withheld error bodies; Git smart HTTP only for the
   claim's repositories; MCP only to the approved URL without caller queries.

Candidates as of 2026-10, judged from upstream documentation:

| Component | Gap |
| --- | --- |
| iron-proxy (Apache-2.0, Go) | Credentials live in configuration and change through a reload; per-attempt issuance and revocation are not described. |
| Infisical Agent Vault | Orchestrator-issued tokens exist, but the project is in preview and time-bound sessions are commercial. |
| superfly/tokenizer | Stateless sealed secrets; a sealed credential cannot be revoked. |
| Envoy with `ext_authz` | Checks authorization once per request, so revocation of open streams needs `ext_proc`, Lua or Wasm; the grant table stays our code and Envoy adds an operational dependency. |
| LiteLLM virtual keys (inference only) | Per-attempt keys need direct access from the sandbox to LiteLLM (ADR 0003) and lose header and error-body scrubbing; caller JWT auth is Enterprise. |

## Decision

Keep `internal/relay` and its specializations. It is not a generic credential proxy:
callers never select an upstream or credential, every grant names one
controller-approved origin, there is no `CONNECT` or TLS passthrough, and the Git
relay's HTTP proxy mode admits only the claim's repositories. It issues no OAuth
tokens and implements no cryptographic primitives: opaque credentials, their SHA-256
lookup keys and the forge relay's name-constrained CA use the Go standard library.

Reuse maintained components at the edges instead of replacing the relay:

- Git and forge credentials stay deployment-owned files re-read per request, so an
  external rotator (for example GitHub App installation tokens or octo-sts) can
  narrow the forge credential without relay changes.
- If agents need package registries or other open egress, add a separate egress
  proxy (for example Smokescreen or iron-proxy) as an execution peer. Do not grow
  the relay into one.

Revisit when a maintained component supports orchestrator-issued, revocable
per-session credentials that cancel open streams, or when a Kubernetes backend (#6)
brings a gateway with external authorization to controller state.

## Consequences

The controller owns about 830 lines of security-critical proxy code, covered by
`internal/relay`, `internal/repo` and `internal/opencode` tests and the identity,
inference and relay fixtures. Each new upstream needs its own closed policy and
adversarial tests. The relay is not production egress control; network policy,
attempt-network TLS and registry access stay deployment-owned.

## References

- [iron-proxy](https://github.com/ironsh/iron-proxy)
- [Infisical Agent Vault](https://github.com/Infisical/agent-vault)
- [superfly/tokenizer](https://github.com/superfly/tokenizer)
- [Envoy ext_authz filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_authz_filter)
- [LiteLLM virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys)
- [Claude Code sandboxing: Git credential proxy](https://www.anthropic.com/engineering/claude-code-sandboxing)
- [Smokescreen](https://github.com/stripe/smokescreen)
