# ADR 0012: Inference identity and credential translation

Status: Accepted
Date: 2026-10-04

## Context

Agent CLIs call inference over native HTTP, independently of MCP. Corporate
gateways may accept OIDC identity or require a long-lived LiteLLM access key.
Neither that key nor provider credentials may enter the task sandbox.

## Decision

Keep inference recipient bindings and advisory catalogs separate from MCP rules and
agent identity bindings. Resolve them from trusted Multica server, workspace and
agent identifiers. Support static configuration with secret-file references and
an authenticated external resolver through the same binding contract. External
infrastructure can store credentials in Vault/OpenBao; do not build a vault.

Deliver only a short-lived, inference-specific identity token to the workload.
Use one of two explicitly configured deployment paths:

- External gateway, preferred: validate identity and active attempt authorization,
  select a server-side LiteLLM key and forward the request with that credential.
  The controller delivers configuration/identity and lifecycle signals only.
- Embedded relay, opt-in: a narrow relay in the existing controller process
  performs the same credential translation using trusted static bindings and
  controller-only secret files, or an external binding resolver. It adds no
  process and does not implement provider routing, model adapters or a key store.

The relay is a reverse proxy, not an HTTP redirect. Validate issuer, signature,
recipient, expiry and the principal's binding to an active attempt before forwarding.
Select credentials and upstream endpoints from trusted bindings; caller-supplied
agent/workspace IDs, URLs, headers or keys cannot choose them. Replace incoming
authorization; do not forward the workload JWT to a key-only upstream. Prevent
credential-bearing redirects and exposure through responses, errors or logs.

Keep model grants, tenant budgets and provider routing in the external inference
service. A relay cannot override its grants or route around them. Stop admitting
requests after attempt completion/cancellation and bound admission during outages.
Define in-flight stream cancellation separately; token expiry alone is not revocation.
Neither deployment path permits a permanent per-agent key inside the sandbox.

For OpenCode, integrate renewal through an adapter-owned local plugin using its
provider auth-loader/fetch hook. Read the projected access token before each
inference request; reject missing tokens without falling back to another key.
The controller renews and atomically projects tokens while the attempt is authorized.
MCP uses the distinct native OAuth store from ADR 0011.

## Consequences

This explicitly permits a narrow embedded translation relay as an opt-in exception
to ADR 0010's external-service default. It enlarges controller responsibility and
credential exposure; keep it a separate module with conformance tests. External
deployments do not require controller access to inference keys.

A disposable OpenCode 1.18.34 experiment verified replacement between two
inference requests in one prompt/tool cycle through a local auth plugin. Updating
options.apiKey, a file substitution or provider auth.json kept the old bearer.
A missing projected token produced no upstream request. Tests used synthetic
tokens and an OpenAI-compatible SSE fixture, not real JWT renewal or LiteLLM.

The experimental external-gateway adapter resolves inference recipients
separately from identity credentials and advisory catalog/default data. Static
bindings and authenticated
external sources use trusted server/workspace/agent references. Operator-approved
URL/issuer pairs bound delivery; changed recipients stop the running attempt.
Model and thinking selection come from the trusted claim, without cached-catalog
authorization. Explicit selections survive absent/stale catalogs; the gateway owns
refusals. An omitted selection requires the catalog default. Catalog changes do
not change identity renewal. Safe explicit effort tokens are preserved as native
reasoningEffort variants; omission uses the model default or no effort.
Fingerprint leases use `inference_url`, distinct from `mcp_url`, and share attempt
revocation/recovery. The dependency-free provider hook replaces authorization
from an atomic token file and leaves global fetch untouched.

The embedded relay is not implemented. Require maintained real issuer/gateway
checks, streaming/cancellation, tenant isolation, credential-leakage tests and
safe offline plugin packaging before advertising production support.

**Model selection follow-up (#29)**

Contract inspection at Multica `b4ca5b4a23e68b26292a680dca7689a952bb1cd5`
found that `handler/daemon.go` supplies `agent.model` and
`agent.thinking_level` in claims, using saved agent configuration at claim time.
The adapter can reuse these fields; a distinct per-task override requires an
upstream contract. OpenCode 1.x maps thinking selection to `--variant`.

Discovery uses `pending_model_list.id` in heartbeat responses and runtime-scoped
result reports. `handler/runtime_models.go` and `handler/runtime_model_catalog.go`
scope requests and cached catalogs to runtime IDs, without an agent reference.
Model entries preserve provider/model IDs and thinking options/defaults, but do
not carry context/output metadata.

Agent-scoped discovery requires upstream request, cache and UI scoping by agent
within its workspace, plus context/output metadata. Do not publish a union of
different agents' catalogs as one runtime catalog. Reuse the pending request/report
mechanism once that scope is represented; a runtime catalog is not authorization.

The adapter separates catalog/defaults from task selections and identity renewal.
A stale catalog must not deny an explicit model selection;
gateway refusals must remain visible without model substitution. Controller
configuration supplies the single Multica origin; shared external resolvers and
attempt authority retain that installation namespace. Agent-scoped upstream
discovery/UI remains pending; scoped local contract tests
do not establish that missing integration.

## References

- [Claim assembly](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/daemon.go)
- [Discovery requests and model entries](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/runtime_models.go)
- [Runtime catalog cache](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/runtime_model_catalog.go)
- [OpenCode selection mapping](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/pkg/agent/opencode.go)

- [OpenCode plugin auth contract](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/plugin/src/index.ts)
- [OpenCode provider integration](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/opencode/src/provider/provider.ts)
- [OpenAI-compatible effort option](https://github.com/vercel/ai/blob/%40ai-sdk%2Fopenai-compatible%402.0.41/packages/openai-compatible/src/chat/openai-compatible-chat-options.ts)
- [LiteLLM custom authentication](https://docs.litellm.ai/docs/proxy/custom_auth)
