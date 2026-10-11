package inference

import (
	"slices"
	"time"
)

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

// RelayPaths are the model surfaces OpenCode providers use: OpenAI-compatible chat,
// OpenAI Responses and Anthropic Messages (with its token counter).
var RelayPaths = []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens"}

// RelayPath admits only RelayPaths; paths are already cleaned.
func RelayPath(path string) bool { return slices.Contains(RelayPaths, path) }

// Attribution is the trusted end-user value: workspace/agent/task from the claim.
func Attribution(workspace, agent, task string) map[string]string {
	return map[string]string{AttributionHeader: workspace + "/" + agent + "/" + task}
}
