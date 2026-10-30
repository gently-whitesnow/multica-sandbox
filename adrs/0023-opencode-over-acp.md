# ADR 0023: Current OpenCode over ACP

Status: Proposed
Date: 2026-10-30

## Context

The adapter supports OpenCode 1.18 through `opencode run --format json`. That stream
reports a tool only after it finishes, so a long silent build trips the 10-minute
idle watchdog (upstream GH 9614), and completion is inferred from step bracketing
with five failure heuristics in `events.go`. OpenCode 2.x makes `run` a thin client
of a resident service and is unsupported here because MCP cannot be delivered to it.
Operators want the current OpenCode release.

Upstream Multica added an ACP transport for OpenCode (`server/pkg/agent/opencode_acp.go`).
The Agent Client Protocol is JSON-RPC over stdio, spoken natively by OpenCode and
through adapters by Claude Code and Codex. ADR 0004 asks for maintained components:
`coder/acp-go-sdk` v0.13.5 is Apache-2.0 with no transitive dependencies. Upstream's
package is not reused: its license restricts embedding.

### Spike findings

`examples/acp-spike` ran `opencode acp` in an attempt-shaped container against
deterministic inference and MCP fixtures on 1.18.35, 2.2.0 and 2.2.2. Its README
holds the results. On every version:

- A tool reports `in_progress` with its input when it starts, 6 s before completion
  in the spike. `stopReason` (`end_turn`, `cancelled`) ends the turn.
- `session/load` in a new process continues the session after replaying its
  history as updates. `session/cancel` answers `cancelled` within tens of
  milliseconds and kills the running tool.
- The projected deny-by-default permissions hold: no permission requests reach the
  client, and a denied tool fails without running. 2.x translates the legacy
  `permission` map (`bash` to `shell`) but still offers denied tools to the model.
- `opencode acp` hosts its server on an attempt loopback port; nothing listens
  after it exits.

On 2.x the native MCP auth store is ignored: a configured remote MCP connects with
no token, and only `mcpServers` in `session/new` carries headers, fixed for the
session. `PromptResponse.usage` covers the turn there; on 1.x it holds the last
model call only, and turn usage comes from `opencode export`. 2.x drops
`acp --cwd` and answers an unknown session with -32602 instead of -32603.

The Docker backend's `Stream` offers no stdin.

## Decision

**Version.** The target is OpenCode 2.2.2, the newest release on 2026-10-30, pinned
by its multi-platform index `sha256:7c8cd91b6055e39306fa9040b9abf4e7d2808b1fccfe2d0389e3f0cdc654a048`.
GitHub still labels 1.19.5 "Latest"; the target follows the newest release, not the
label. No floating tag is used. The spike lists it as a candidate; it enters
`images.txt`, and so `Supported`, only when the conformance suite passes on the
ACP adapter. Newer releases follow the same path: a script resolves the release's
index digest, runs the spike and conformance, and proposes the `images.txt` change.

**Transport.** The adapter drives `opencode acp` with `coder/acp-go-sdk`:

- `ProjectedRun` gains a bidirectional exec with `Stream`'s env, user and output
  bounds; the Docker backend implements it with `docker exec -i`.
- The client offers no filesystem or terminal and rejects permission requests
  once; the projected configuration remains the permission authority.
- Updates map to Multica messages by `toolCallId`; only the `tool_call` frame names
  the tool. Child-session updates keep tool calls and drop text, like upstream.
  The idle watchdog pauses while a tool is `in_progress`.
- `stopReason` decides the outcome; an exit without a prompt response stays the
  retryable provider-network failure.
- Resume uses `session/load` in the retained workdir and ignores the replay. Any
  load error retires the session and runs fresh once.
- Cancellation sends `session/cancel` before the exec is torn down.
- Usage comes from `PromptResponse.usage` where a version is verified to report
  the whole turn, else from the session export through `Capture`.

**MCP relay.** MCP moves to a controller relay grant, as ADR 0012 and 0014 do for
inference and the Multica API. `session/new` names each selected connection with
the relay's attempt alias and an opaque `mcp_relay_<hex>` bearer. The relay
forwards to the exact approved URL with the attempt's current access token from
the existing renewal loop, registered with the attempt authority before use
(ADR 0009, 0011). It carries streamable HTTP (POST, SSE responses, the GET
stream, `Mcp-Session-Id`), refuses redirects, and revocation at cleanup cancels
open streams. Access tokens no longer enter the attempt; the auth-store
projection and its file go.

**1.x.** Listed 1.x images run on the same transport and relay. The `run`
transport is removed once ACP conformance passes, keeping only operator commands.

## Consequences

The adapter loses the step-bracketing heuristics and false idle failures, follows
current OpenCode, and shares an ACP client with future Claude Code and Codex
adapters. It gains one dependency, a stdin path in the backend contract, an MCP
relay policy and a conformance gate per release. Releases arrive almost daily, so
the pinned version lags the newest by the time a run of the gate takes.

ACP and OpenCode 2.x move fast. Usage and child-session handling rely on behavior
the spike observed, not documented contracts; conformance must check them on every
candidate. Until the relay lands, 2.x stays out of `images.txt`.

## References

- [Agent Client Protocol](https://agentclientprotocol.com)
- [coder/acp-go-sdk](https://github.com/coder/acp-go-sdk)
- [Upstream OpenCode ACP backend](https://github.com/multica-ai/multica/blob/main/server/pkg/agent/opencode_acp.go)
- [OpenCode v2.2.2](https://github.com/anomalyco/opencode/releases/tag/v2.2.2)
- [Spike](../examples/acp-spike/README.md)
