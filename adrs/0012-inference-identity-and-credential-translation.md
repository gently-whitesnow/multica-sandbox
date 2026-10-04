# ADR 0012: Inference identity and credential translation

Status: Accepted
Date: 2026-10-04

## Context

Agent CLIs call inference over native HTTP, independently of MCP. Corporate
gateways may accept OIDC identity or require a long-lived LiteLLM access key.
Neither that key nor provider credentials may enter the task sandbox.

## Decision

Keep inference endpoint/model bindings separate from MCP authorization rules and
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

Renewal, dynamic bindings and the embedded relay are not implemented. Require
maintained adapter tests, real issuer/gateway checks, streaming/cancellation,
tenant isolation, credential-leakage tests and safe offline plugin packaging
before advertising either path as production support.

## References

- [OpenCode plugin auth contract](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/plugin/src/index.ts)
- [OpenCode provider integration](https://github.com/anomalyco/opencode/blob/aec0b9a6d8898f68f923aaf08b7306d931fd9d76/packages/opencode/src/provider/provider.ts)
- [LiteLLM custom authentication](https://docs.litellm.ai/docs/proxy/custom_auth)
