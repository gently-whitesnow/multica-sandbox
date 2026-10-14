# Workspace inference and model selection

ADR 0012: the inference principal is the workspace. An external OpenAI-compatible
gateway such as LiteLLM owns model access, budgets, limits and provider routing
through the workspace's key. The controller relays attempts to it and maps trusted
Multica `agent.model` / `agent.thinking_level` from the claim into native OpenCode.
Those fields express selection, not permission. MCP identity (ADR 0011) is separate.

## Configuration

Set `opencode.inference_file` to an absolute path and `opencode.inference_relay`
(`listen`, `url`); one is invalid without the other. Add the controller to the
execution template `peers` with the alias named by `url`, as for `multica_relay`.
Keep the gateway on the control network, off attempt networks.

The file (`deploy/inference.example.json`) holds approved gateway origins
(`gateways`) and either static `bindings` or an authenticated `external` resolver:

- a static binding has `workspace_id`, `gateway` (an approved origin, optionally with a clean base path)
  and `key_file`, an absolute controller-only file with the key. The file is
  re-read for each new attempt, so a rotated key applies to new attempts only;
- an external resolver receives `{version: 1, workspace: {server, workspace_id}}`
  with a bearer from `bearer_file`. It returns the echoed `workspace` and
  `target: {gateway, key}`. Unknown fields, mismatched references, unapproved
  gateways, empty keys, oversized responses and redirects deny the attempt.

The controller's `server` supplies the Multica origin; bindings cannot override it.
Keys are a redacted type: they never format, marshal or reach logs, results or
attempt files.

## Relay

At attempt start the adapter resolves the binding and issues one opaque
`inf_relay_<64 hex>` credential. OpenCode's `managed-inference` provider gets
`baseURL: <inference_relay.url>/v1` and that credential as `apiKey`; nothing
rotates. The relay (`internal/relay`) forwards only `/v1/chat/completions`,
`/v1/responses`, `/v1/messages` and `/v1/messages/count_tokens` to the binding's
gateway. It accepts the credential as a bearer token or `X-Api-Key` (Anthropic
clients), removes both, sets `Authorization` to the key and streams SSE
responses. It refuses redirects and bounds bodies and in-flight requests. Upstream
error bodies are replaced by a fixed body with the same status. Cleanup or a
controller restart revokes the grant and cancels in-flight streams.

Attribution: the relay sets `X-Litellm-End-User-Id: <workspace>/<agent>/<task>`
from the claim and removes all caller `x-litellm-*` request headers. LiteLLM reads
that header before body `user`/metadata fields and records it as `end_user`
(`LiteLLM_EndUserTable`, `LiteLLM_DailyEndUserSpend`, spend logs) beside the key
hash. Callers cannot spoof it.

## Catalogs and selection

Catalog data is advisory and runtime-scoped: static `catalogs` keyed by
`workspace_id`, `catalog_external` or `gateway_catalog` (at most one). Entries include `default_model` and a model
map with context/output preparation metadata, optional `thinking.supported_levels`
(value/label pairs) and `default_level`, `api` (`chat` default, `responses`,
`messages`) and advisory `cost` (USD per million: `input`, `output`, `cache_read`).
These are capabilities, not grants.

`gateway_catalog: {ttl, default_model}` reads LiteLLM `/v1/model/info` with each
workspace's key, so the catalog is exactly what the key may use (up to 512 models).
Anthropic models get `messages`, OpenAI models `responses`, others `chat`; non-chat
modes are skipped. Limits, reasoning support and prices come from the gateway;
deployment parameters are discarded. Reasoning levels are LiteLLM's
`reasoning_effort_levels` when set; otherwise `low`/`medium`/`high` (Messages:
`high`/`max`) plus or minus each `supports_<level>_reasoning_effort` flag, with
`default_reasoning_effort` as the default. Messages keeps levels with a budget. A catalog is reused for `ttl` (default 5m),
and a failed refresh keeps the last good one.

The model's `api` selects the OpenCode provider and picker prefix:
`managed-inference/` (`@ai-sdk/openai-compatible`, chat), `inference-codex/`
(`@ai-sdk/openai`, Responses) and `inference-claude/` (`@ai-sdk/anthropic`,
Messages). Native APIs keep prompt caching and reasoning state between turns.
Only the selected model is projected into `opencode.json`.
Catalog sources receive `{version: 1, workspace: {server, workspace_id}}` and
return the echoed `workspace` and `catalog`. A catalog outage does not deny an
explicit selection or the binding.

An explicit model is passed through even if it is absent from the catalog or the
catalog lookup fails. A known provider prefix is stripped and selects the API, so
legacy `managed-inference/` selections stay on chat; otherwise the name is the
gateway alias verbatim and the catalog entry selects the API. Unknown models use native OpenCode defaults. An
omitted model requires a catalog default and fails visibly without a fallback.
A safe explicit thinking token maps to a native variant with `reasoningEffort`;
for Messages, `low`/`medium`/`high`/`max` map to extended-thinking budgets and other
tokens are denied.
Omission uses the selected model's advertised default, or sends no effort.

Discovery reuses Multica heartbeat pending requests (woken by the daemon WebSocket) and model result reports in
upstream's shape. The controller registers one runtime per workspace, so the
workspace comes from the registered runtime. Entries use IDs
`<provider>/<gateway-model>`, labels, default and thinking options; upstream
entries have no context/output. The gateway refuses ungranted models or exhausted
budgets at task time. Multica shows the numeric HTTP status without substitution.

## Verification

`VERIFY_INFERENCE=1 ./verify.sh` runs native OpenCode through the relay to pinned
LiteLLM with Postgres-backed virtual keys and a deterministic provider; no
subscription is needed. Add `VERIFY_SERVICE=1 VERIFY_OPENCODE=1` for real Multica
claims through the Compose controller. ADR 0012 lists the adversarial coverage.
