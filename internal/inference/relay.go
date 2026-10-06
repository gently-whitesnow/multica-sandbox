package inference

import "time"

// RelayPrefix marks the opaque per-attempt inference credential.
const RelayPrefix = "inf_relay_"

// AttributionHeader is LiteLLM's standard end-user header. LiteLLM reads it before
// the body `user`/metadata fields; the relay strips every caller `x-litellm-*` header
// (including the earlier-checked `x-litellm-customer-id`) and sets this one from the claim.
const AttributionHeader = "X-Litellm-End-User-Id"

// ReservedHeaders are gateway headers only the relay sets or reads.
var ReservedHeaders = []string{"x-litellm-"}

// HeaderTimeout lets a reasoning model think before its first streamed byte; it
// matches the adapter's 10-minute idle watchdog.
const HeaderTimeout = 10 * time.Minute

// RelayPath admits the OpenAI-compatible chat surface OpenCode uses; paths are already cleaned.
func RelayPath(path string) bool { return path == "/v1/chat/completions" }

// Attribution is the trusted end-user value: workspace/agent/task from the claim.
func Attribution(workspace, agent, task string) map[string]string {
	return map[string]string{AttributionHeader: workspace + "/" + agent + "/" + task}
}
