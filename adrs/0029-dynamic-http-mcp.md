# ADR 0029: Dynamic remote HTTP MCP connections

Status: Accepted.
Date: 2026-11-01

## Context

Multica v0.7.2 already provides a workspace MCP library, remote URL/header forms,
agent assignments and a fresh effective configuration on every claim. Sandbox
previously rejected custom headers and required an operator URL list for every
connection, causing unnecessary worker deployments.

The owner requested native configuration as the user interface, arbitrary remote
MCPs and optional credentials via headers or agent ENV. Existing Ecosystem JWTs
must not be automatically delivered to a user-selected foreign recipient.

## Decision

Reuse native Multica Settings → MCP and agent assignment. Parse legacy
`mcpServers` and native `mcp` remote shapes with their enabled toggles. A changed
connection applies on the next attempt, including a resumed conversation. No
worker restart, new image or new connection-management service is needed.

Support Streamable HTTP first. An anonymous server needs its URL; an authenticated
server can use normal headers such as Authorization, API key or Cookie. Optional
`{env:NAME}` interpolation reads only the named agent ENV in the trusted claim,
never controller process ENV. Missing variables produce safe errors naming the
variable, not its value. Consumed variables are not projected into attempts while ordinary ENV passes without a controller allowlist (ADR 0030). Per-server process ENV belongs to
future stdio support and is not silently treated as HTTP authorization.

Keep real headers and the selected URL in the controller. OpenCode receives the
existing relay alias and opaque per-attempt credential. Formatting and JSON redact
connection URLs/headers. Validate header names/values, bounds and hop-by-hop or
proxy/authority fields. Incoming caller credentials never choose authorization.
Preserve the selected URL's fixed query exactly, discarding added caller query.
No redirects or userinfo in endpoint URLs.

One deployment setting, `opencode.mcp_public_proxy`, enables selected public
endpoints through trusted HTTP(S) egress. The origin is operator configuration,
never chosen by an MCP. No direct fallback is permitted, including failed CONNECT.
The proxy/network layer must deny host/private/metadata destinations and DNS
rebinding, with only deployment-owned exceptions. A unique execution-network
alias avoids selecting an isolated control-network endpoint by DNS.

Identity is selected explicitly with `Authorization: Bearer {iam:access_token}`
in native MCP headers (ADR 0030). Other anonymous URLs and user-header connections
receive no managed JWT. All use the configured proxy; the identity marker does
not change network reachability. Deployment recipient URL lists are removed.

Public/static-header failures retain status while withholding upstream error
bodies, headers, trailers and redirect locations. Revocation cancels streams and
ends the opaque grant. Each attempt uses only its own selected connections and
credentials; another attempt's headers, query or selected aliases are not reused.

OAuth/browser login, stdio, broker connections and per-server timeouts are
explicitly unsupported in this release. OAuth belongs in a future backend flow
with persistent token refresh, rather than interactive login in disposable agents.
Multica's native human-only library/assignment gates remain in force. This change
does not introduce a sandbox permission engine or bypass those native gates.

## Alternatives

A URL allowlist per MCP recreates the rollout problem. Sending managed JWTs to
arbitrary URLs leaks identity to unintended recipients. Putting user secrets in
OpenCode config/ENV exposes them to agent tools unnecessarily. Another UI, vault
or OAuth implementation duplicates components that native Multica already owns.

## Consequences

The normal user flow is URL plus optional header, agent assignment, then the next
message. Operators configure public egress once. Private endpoints still need a
valid network route; accepting a native MCP configuration does not grant network
or external service permissions. User-provided expiring tokens require refresh;
this release does not promise a complete interactive OAuth flow.

Conformance must cover real native tool invocation and configuration changes,
secret absence, proxy/DNS/private refusal, query/redirect/error leaks, managed
identity preservation, attempt separation and ended grants before advertising the
adapter capability. Tests use isolated fake MCP/provider services, not owner tasks.

## References

- [Native MCP form](https://github.com/multica-ai/multica/blob/f05a525ac8e82f5e9169ad078e44f166fad6d5c8/packages/views/agents/components/tabs/mcp-server-dialog.tsx)
- [MCP authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
- `internal/opencode/mcp_selection.go`, `mcp_headers.go`, `public_mcp.go`
- `internal/relay/relay.go`, ADR 0023, ADR 0027
- `scripts/test-dynamic-mcp.sh`, `integration/mcp_library_test.go`
