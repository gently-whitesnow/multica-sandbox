# ADR 0012: Workspace inference and gateway metadata

Status: Accepted; refined 2026-11-01.
Date: 2026-10-05

## Context

Inference gateways own provider connections, available models and budgets. The
sandbox must use the workspace's gateway key without exposing it to agents.
Multica discovers models per runtime, which is registered per workspace.

The early per-agent inference JWT prototype duplicated gateway authorization.
The subsequent static catalog invented 128k context and 16k output limits. These
values affect real OpenCode preparation and compaction, not only display labels.

## Decision

The inference principal is the workspace. Model access, budgets and provider
routing belong to the gateway. The controller neither runs a provider router nor
implements a second inference authorization service.

Enable the shared credential relay with `opencode.inference_file` and
`opencode.inference_relay`. Resolve the gateway origin and key from a static
workspace binding, an authenticated external resolver, or workspace Multica env
(ADR 0022). The controller supplies its Multica origin; callers cannot change
workspace scope, upstream or key. Keys are controller-only, redacted values.
A rotated key applies to new attempts; running grants keep their original key.

Each attempt receives an opaque `inf_relay_` credential. OpenCode uses the relay
URL and that credential; the relay substitutes the real key for the configured
gateway. It forwards only chat completions, Responses, Messages and token counting.
Caller credentials and attribution are stripped. Trusted workspace/agent/task
attribution is derived from the claim. Native provider errors retain their HTTP
status but withhold bodies, headers and trailers that may contain credentials.
Cleanup or controller restart revokes the grant and cancels in-flight requests.
The gateway stays off attempt networks.

Trusted claim model and thinking selections map to OpenCode's native providers:
chat to `managed-inference`, Responses to `inference-codex`, Messages to
`inference-claude`. Native APIs retain provider caching/reasoning. Only the
selected model is projected. An explicit safe model absent from discovery still
reaches the gateway, which decides whether the key may use it; catalog data is
preparation metadata, not permission.

Use at most one catalog source: static `catalogs`, `catalog_external`, or
`gateway_catalog`. Static values are explicit operator overrides. Context/output
are optional; absent values are omitted from OpenCode configuration rather than
inventing numbers or sending zeros that disable native compaction. OpenCode owns
the fallback for unknown metadata.

Gateway discovery uses the same workspace key, caches results for a TTL and
retains the last good catalog when refresh fails. Concurrent callers of one
workspace share a single refresh; no lock spans gateway I/O, so a slow gateway
delays only its own workspace:

- `format: litellm` (default) reads `/v1/model/info`. Native provider identity,
  limits, reasoning levels and prices are mapped; deployment parameters are not.
- `format: cliproxy` joins `/v1/models` with `/v1/models?client_version=`.
  The empty query selects CLIProxyAPI's existing rich client response. Match exact
  IDs, preserving pool aliases. Ordinary ownership selects the wire API; rich
  `context_window`, `max_tokens` and reasoning supply effective preparation
  metadata. Skip hidden/unsupported entries and discard provider instructions.

Neither format requires a management key, gateway fork or separate model registry.
A catalog outage does not invalidate an explicit selection or inference binding.
Gateway discovery returns at most 512 entries; static overrides retain their
existing bound. These bounds keep discovery usable rather than granting models.

OpenCode performs compaction. Optional `opencode.compaction_max_tokens` uses its
native buffer to cap the trigger while preserving true model metadata and earlier
small-model thresholds. The deployment defaults to 400k; 0 keeps native behavior.
This requires verified 2.x semantics and is not a spending/request quota. Tool
output can overshoot and summarization consumes tokens. Older images reject a
positive cap rather than silently ignoring it.

## Consequences

New gateway models and changed limits no longer require a per-model deployment
edit. The provider/gateway registry owns capabilities. Pool aliases remain exact
so two subscriptions are not silently substituted. Unknown metadata uses native
OpenCode behavior, which can differ between verified releases.

The controller still holds the gateway key and requires trusted origins and
network placement. Catalog metadata cannot grant model access or establish an
accurate billing report. Gateway auth-stage refusals may not be attributed to a
specific end user. Native retries and HTTP status reporting remain unchanged.

**Verification.**

Unit contracts cover catalog scoping, cache/outage behavior, malformed and partial
metadata, redirects, hidden models, exact aliases and discarded instructions.
Native OpenCode/LiteLLM fixtures cover workspace-key isolation, unknown explicit
models, quota refusals, usage attribution, missing limits and grant revocation.
No provider credentials or paid generation are needed by deterministic fixtures.
Actual deployment acceptance checks gateway metadata and configured role models
without modifying owner business tasks.

## References

- [CLIProxyAPI catalog selection](https://github.com/router-for-me/CLIProxyAPI/blob/ef9e71201e0ca72b03a540d9b6e0862f8c053347/sdk/api/handlers/openai/openai_handlers.go)
- [OpenCode native model defaults](https://github.com/anomalyco/opencode/blob/v2.2.2/packages/schema/src/model.ts)
- [LiteLLM virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys)
- `internal/inference`, `internal/opencode/selection.go`, `scripts/test-inference.sh`
