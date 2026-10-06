# Managed inference identity and model selection

The optional external OpenAI-compatible gateway owns model access, quotas, budgets
and provider routing. The controller delivers short-lived identity and maps trusted
Multica `agent.model` / `agent.thinking_level` from the claim into native OpenCode.
Those fields express selection, not permission. MCP uses its separate OAuth store.
Migration: remove `server` from identity/inference files and move binding `model` /
`models` into a separate catalog/default entry; recipient bindings keep URL/issuer.

Set `opencode.inference_file` to an absolute configuration path. It names an
`identity_file`, approved `gateways` (URL/issuer pairs), and either static `bindings`
or an authenticated `external` recipient resolver. Bindings contain only
workspace/agent IDs, URL and issuer. The controller's `server` supplies the Multica
origin to both services; identity/inference files cannot override it.

Catalog/default data is separate and runtime-scoped: use static `catalogs` keyed by
`workspace_id` (a service catalog snapshot) or `catalog_external`. Entries include
`default_model` and a model map with context/output preparation metadata and optional `thinking.supported_levels`
(value/label pairs) and `default_level`. These values are capabilities, not budgets
or a controller permission list. Catalog lookup neither issues JWTs nor grants
access, and catalog changes do not interrupt identity renewal.

Recipient sources receive `{version: 1, agent: {server, workspace_id, agent_id}}`
and return the exact echoed reference and `target: {url, issuer}`. Catalog sources
receive `{version: 1, workspace: {server, workspace_id}}` and return the echoed
`workspace` and `catalog`. Agent-scoped catalog entries are rejected.
Unknown fields, credentials, mismatched references, oversized responses and
redirects fail the relevant resolution. The approved URL/issuer boundary remains
mandatory for identity delivery; the catalog is advisory.

An explicit model is passed through the managed provider, even if absent from the
catalog or catalog lookup fails. `managed-inference/` qualification is stripped
before gateway delivery; otherwise the model name is the gateway alias verbatim.
For unknown models, zero metadata uses native OpenCode unknown-context and
output defaults; the controller invents no capability limit. An omitted model requires a
catalog default; absence fails visibly without another provider/model fallback.

A safe explicit thinking token is mapped to a native variant with `reasoningEffort`
and preserved even when not listed in discovery. Omission uses the selected
model's advertised thinking default, or sends no effort for models without a
control. The gateway decides whether an explicit effort is acceptable. The adapter
never silently drops it. Only token syntax is checked locally.

Discovery reuses Multica heartbeat pending requests and model result reports in
upstream's shape (`status`, `supported`, `models`, or `failed` with `error`).
Upstream `b4ca5b4` sends only a request ID and caches by runtime; the controller
registers one runtime per workspace, so the workspace comes from the registered
runtime and the server from controller configuration. Entries use IDs
`managed-inference/<gateway-model>`, labels, default and thinking options/defaults;
upstream entries have no context/output, which stay in native OpenCode metadata.
Agents of one workspace see the same advisory list; the gateway refuses
ungranted models at task time and Multica shows that failure without substitution.
Catalog outage reports a failed discovery without detail.

The native provider auth hook reads atomically replaced mode-0600
`/workspace/inference-token.json` before each request. It replaces authorization,
refuses redirects and rejects missing/malformed/expired identity. The native auth
store contains only a non-secret loader marker. Global fetch remains unchanged.
Identity refresh pins only the URL/issuer; model selection is fixed for each task.

Fingerprint leases use `inference_url`, separately from `mcp_url`; renewal is
bounded by JWT expiry and 15 seconds. End/cancellation/failure revokes the attempt
and removes the workload. Already admitted upstream work may continue. Native
provider retries remain native. Gateway failure events expose only a numeric HTTP
status in Multica; bodies, headers and credentials remain withheld.

`VERIFY_INFERENCE=1 ./verify.sh` runs OpenCode, Keycloak and LiteLLM with a
deterministic provider; no subscription is needed. Add `VERIFY_SERVICE=1
VERIFY_OPENCODE=1` for actual Multica claims through the Compose controller.
Full event/usage/session reporting and production conformance remain #24.
