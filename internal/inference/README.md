# Managed inference identity

The persistent OpenCode adapter optionally uses an external OpenAI-compatible
inference gateway. MCP selection stays in Multica's effective `mcp_config`.
Inference endpoint/model bindings have a separate controller-owned configuration;
claims cannot supply endpoints, model grants or credentials.

Set `opencode.inference_file` to an absolute configuration file. It names a separate
`identity_file` using the shared identity resolver contract, approved `gateways`
(URL/issuer pairs), and either static `bindings` or one authenticated `external`
resolver. Each binding uses the trusted server/workspace/agent reference, one
selected model and a model-to-context/output-limits map. These limits describe
OpenCode models; the external gateway owns actual model grants and budgets.

External resolution POSTs `{version: 1, agent: {server, workspace_id, agent_id}}`.
The response must echo version and the exact agent, with `target` containing
`url`, `issuer`, `model`, and `models`. Unknown fields, unapproved URL/issuer pairs,
ungranted selected models, oversized responses, redirects and outages are denied
before issuance. Static bindings and returned model maps are copied. Credentials
are resolved separately through `identity_file`; changing an endpoint or model
binding during an attempt stops it instead of silently reconfiguring the provider.

The native OpenCode provider auth-loader hook reads the mode-0600 atomically
replaced `/workspace/inference-token.json` before each request. The adapter writes
a dependency-free local module, with no global fetch patch. It uses the bundled
`@ai-sdk/openai-compatible` provider, permits the configured chat-completions route,
replaces authorization, refuses redirects and rejects missing/malformed/expired
identity. The native provider auth store contains only a non-secret loader marker;
it cannot serve as a fallback credential. OpenCode's global config directory stays
read-only for offline startup.

Inference leases use `inference_url` in the external authority protocol, separately
from `mcp_url`. Register each fingerprint before projection, renew each second,
and bound the lease by JWT expiry and 15 seconds. Completion, cancellation or
renewal failure revokes every fingerprint for the attempt and removes its workload.
The gateway must validate JWT recipient and active attempt admission for every
request. Ending admission does not by itself promise cancellation of upstream
work already in flight. Provider keys and gateway administration credentials stay
outside workloads; provider routing stays in the gateway. No embedded relay is
part of this implementation.

`VERIFY_INFERENCE=1 ./verify.sh` runs native OpenCode with real Keycloak and pinned
LiteLLM. The provider behind LiteLLM is deterministic and checks a gateway-only
key, so the test needs no subscription. Add `VERIFY_SERVICE=1 VERIFY_OPENCODE=1`
for the real Multica claim through the Compose controller. Full events/usage/session
and production adversarial conformance remain #24.
