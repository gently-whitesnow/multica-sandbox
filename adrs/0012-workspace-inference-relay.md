# ADR 0012: Workspace inference keys through the controller relay

Status: Accepted
Date: 2026-10-04; rewritten 2026-10-06 (#36)

## Context

Agent CLIs call inference over native HTTP, independently of MCP. Corporate
gateways such as LiteLLM grant models, budgets and limits to long-lived virtual
keys. Neither those keys nor provider credentials may enter the task sandbox.
The first prototype issued controller-renewed inference JWTs per agent to a
JWT-verifying gateway. That required a gateway custom-auth hook, an OpenCode auth
plugin and a token renewal loop. It also scoped access per agent, while Multica
discovers and caches model catalogs per runtime, which is one per workspace (#29).
Nobody uses the product yet.

## Decision

The inference principal is the **workspace**. Model access, budgets and limits
belong to the workspace's gateway key and stay in LiteLLM or the gateway. The
controller neither stores keys in a vault nor routes providers.

There is one path: the embedded relay from ADR 0014 (`internal/relay`), enabled by
`opencode.inference_file` together with `opencode.inference_relay`. A binding maps
a workspace to an approved gateway origin and its key:

- static: `workspace_id` → `gateway` + controller-only `key_file`, re-read for
  each new attempt;
- external: an authenticated resolver receives
  `{version: 1, workspace: {server, workspace_id}}` and returns the echoed
  `workspace` and `target: {gateway, key}`.

`server` comes from controller configuration and `workspace_id` from the claim.
Agent IDs, URLs, headers and request bodies never select the upstream or the key.
The gateway must be in the configured `gateways` list. Keys are a redacted type
that never formats, marshals or logs.

At attempt start, the adapter resolves the binding and issues one opaque
`inf_relay_<64 hex>` credential bound to the gateway origin, the key and trusted
attribution. OpenCode receives it as the managed provider's `apiKey`, with
`baseURL` set to the relay's peer alias plus `/v1`. Nothing rotates. Cleanup revokes
the grant before the agent process is joined, which cancels in-flight streams.
A controller restart loses every grant and fails closed. A changed key applies to
new attempts only.

The inference relay forwards only `/v1/chat/completions` and waits up to the
adapter's 10-minute idle bound for response headers. It keeps SSE flushing and the
shared body, in-flight and redirect limits. It replaces `Authorization` and removes
every caller `x-litellm-*` header. It strips
`x-litellm-*` response headers, which carry spend and provider details. Upstream
error bodies are replaced by a fixed JSON body with the same status, so key
fragments in gateway errors do not reach the workload. The Multica relay keeps
passing API errors through.

**Trusted attribution.** The relay sets `X-Litellm-End-User-Id:
<workspace_id>/<agent_id>/<task_id>` from the claim. LiteLLM reads its standard
customer headers before the body `user`, `litellm_metadata.user` and
`metadata.user_id` fields. It checks `x-litellm-customer-id` first, which the
relay strips together with other caller `x-litellm-*` headers. LiteLLM records the
value as `end_user` (`LiteLLM_EndUserTable`, `LiteLLM_DailyEndUserSpend`, spend
logs), next to the workspace key hash. Callers cannot spoof it.

Model selection is unchanged from #30/#34: trusted claim `agent.model` /
`agent.thinking_level` map to the native model and `reasoningEffort` variant.
Catalogs stay advisory and workspace-scoped (`catalogs[].workspace_id`), so the
Multica picker lists what the workspace key may use. The gateway refuses ungranted
models or exhausted budgets. Multica shows the numeric status without model
substitution.

## Consequences

The controller holds workspace keys in memory during attempts and is the only
inference egress for workloads. It is an approved peer on attempt networks, and
the gateway stays off them. Production egress policy and TLS remain
deployment-owned. Keycloak and attempt leases remain for MCP (ADR 0011) only.
Per-agent inference grants are not expressible; a deployment that needs them
must split agents across workspaces.

Removed without compatibility: controller-issued inference JWTs and renewal,
`inference_url` attempt grants, the OpenCode auth plugin, `inference-token.json`,
the `auth.json` marker and their projection paths, the separate inference
identity file and the JWT/Keycloak/custom-auth fixtures.

Verified at Multica `b4ca5b4` with LiteLLM v1.104.0 (Postgres-backed virtual keys)
and OpenCode 1.18.34:

- unit: cross-workspace grants, ended, forged and upstream credentials, restart,
  caller attribution stripping, error and header withholding, SSE flushing and
  revocation of in-flight streams, key re-read and redaction;
- `VERIFY_INFERENCE=1`: native OpenCode through the relay to real LiteLLM. It
  covers two workspaces with distinct keys and spoofing through headers and body.
  Another workspace's credential is refused a model only this workspace's key
  grants. Upstream, master, provider, forged and ended credentials are denied.
  Keys stay out of attempt env and files, and LiteLLM is unreachable from the
  attempt. A key change to a zero-budget key yields HTTP 422 for new attempts
  while the running one continues. Revocation ends both relay and gateway streams.
  An explicit model absent from the catalog is forwarded; an ungranted model
  fails with HTTP 403;
- `VERIFY_SERVICE=1 VERIFY_OPENCODE=1 VERIFY_INFERENCE=1`: a real Multica claim
  through the Compose controller, the upstream CLI and LiteLLM. It also covers
  usage attribution, key absence from logs, transcript, comments and results,
  runtime discovery with model/reasoning selection, HTTP 403/422 refusals and a
  controller restart that revokes a live grant.

LiteLLM does not record auth-stage refusals (ungranted model, exhausted budget
before a call) per end user. Fixtures check that no other model was attributed.
Model entries in upstream discovery still lack context/output metadata, and
agent-specific pickers would need upstream request/cache/UI scoping. Neither is
needed now (#29).

## References

- [Claim assembly](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/daemon.go)
- [Discovery requests and model entries](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/runtime_models.go)
- [Runtime catalog cache](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/runtime_model_catalog.go)
- [OpenCode selection mapping](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/pkg/agent/opencode.go)
- [OpenAI-compatible effort option](https://github.com/vercel/ai/blob/%40ai-sdk%2Fopenai-compatible%402.0.41/packages/openai-compatible/src/chat/openai-compatible-chat-options.ts)
- [LiteLLM virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys)
- [LiteLLM customer/end-user tracking](https://docs.litellm.ai/docs/proxy/users)
