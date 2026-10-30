# OpenCode over ACP spike

Drives `opencode acp` in a disposable container through
[`coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk) (Apache-2.0, no transitive
dependencies) and checks what the transport offers the OpenCode adapter. Decision
input for [ADR 0023](../../adrs/0023-opencode-over-acp.md); not a conformance test.
Candidates are official releases pinned by index digest in `main.go`; they enter
`images.txt` only after the adapter's conformance suite passes on them.

```sh
go run ./examples/acp-spike                  # newest verified image in images.txt
go run ./examples/acp-spike -version 2.2.2   # pinned candidate (main.go)
go run ./examples/acp-spike -image IMAGE@sha256:DIGEST -acp-mcp -debug
```

Requires Docker and loopback fixtures reachable as `host.docker.internal`
(Docker Desktop, or `--add-host` on Linux). No provider or account is contacted:
a deterministic OpenAI-compatible fixture scripts the model turns and a local MCP
fixture records the bearer of every request.

The container mirrors the attempt: UID 65532, read-only rootfs, tmpfs workspace,
no capabilities, `no-new-privileges`, the adapter's `opencode.json` (deny-by-default
permissions, remote MCP, fixed provider key) and its native MCP auth store.

## Results (2026-10-30, Docker Desktop arm64)

| Check | 1.18.35 | 2.2.0, 2.2.2 |
| --- | --- | --- |
| `loadSession`, HTTP MCP advertised | yes | yes |
| Tool `in_progress` with input before completion | yes, 6.04 s early | yes |
| MCP token from native auth store | yes | no (empty bearer) |
| Auth store rewritten mid-turn is used | yes | no |
| `mcpServers` headers in `session/new` | yes | yes (static) |
| `PromptResponse.usage` covers the turn | no, last call only | yes |
| Usage from session export | `opencode export` | `session export --standalone` runs; shape not parsed |
| `session/load` in a new process continues | yes | yes |
| `session/cancel` → `cancelled`, tool killed | 26 ms | 12 ms |
| Permission requests under the projected config | none | none |
| Offered but denied tool fails without running | yes | yes |

Differences on 2.x: it translates the legacy `permission` map (and `bash` to `shell`)
but still offers denied tools such as `opencode_worktree_remove`; no `acp --cwd` flag, the shell tool is `shell`, an unknown
session fails with -32602 instead of a generic -32603. Updates name the tool only
on the `tool_call` frame; 1.x titles later updates with the command, so the
adapter keys calls by `toolCallId`. `session/load` replays the history as updates.
