# Experimental OpenCode MCP adapter

The persistent controller's opt-in OpenCode path projects trusted Multica task context,
drives `opencode acp` over ACP (ADR 0023) and relays selected remote MCP connections
with rotating verified identity during one disposable attempt. User-owned images are
inspected once at startup ([image contract](#image-contract)). Optional inference uses
the workspace-key relay (ADR 0012). See [conformance](#conformance) for pinned versions,
tests and limitations.

Remote MCP configs are read per claim: anonymous, static/agent-ENV headers, or explicit
`Authorization: Bearer {iam:access_token}` for rotating identity. Attempts receive only
opaque grants ([ADR 0029](../../adrs/0029-dynamic-http-mcp.md), ADR 0030).

## Configuration

Start from `deploy/controller.example.json` and `deploy/opencode.example.json` with a
digest-pinned OpenCode image. Mount identity configs, secret files and authority
credentials **only into the controller** via a local Compose override; keep secrets
outside Git.

- **`mcp_relay` (`listen`, `url`):** required by `identity_file` or `mcp_public_proxy`.
  Attempts use opaque `mcp_relay_` grants; access tokens stay in the controller. Add
  the controller alias to template `peers`; keep MCP services on the control network.
  Forwards streamable HTTP (POST, SSE, GET stream, `Mcp-Session-Id`) to exact approved
  URLs, preserves configured query params (discarding caller query), refuses redirects,
  strips `WWW-Authenticate` and cancels streams at cleanup. Legacy SSE and encoded paths
  are unsupported.
- **`network` / `peers`:** operator-owned internal bridge template containing only
  approved `peers`. Each attempt gets an isolated internal network. Enforce host and
  metadata egress policy outside Docker.
- **Command & inference:** defaults to `opencode acp` in `/workspace/work` (operator
  commands must speak ACP on stdio). No provider credentials in images/commands; managed
  inference uses `inference_file` and `inference_relay` ([`internal/inference`](../inference/README.md)).
- **Remote MCP selection:** accepts `mcpServers` and native `mcp` remote shapes,
  toggles, fixed query params and headers. `{env:NAME}` reads agent ENV only (never
  attempt ENV or IAM secrets); `Authorization: Bearer {iam:access_token}` requests a JWT.
  Unsupported shapes (stdio, browser OAuth, broker, >16 servers, or non-IAM without
  `mcp_relay`) skip with a warning and Sandbox Notice; malformed configs, URL templates,
  reserved headers, missing ENV or unservable IAM reject the task.
- **`mcp_public_proxy`:** deployment-owned HTTP(S) proxy origin denying private/host/metadata
  destinations (including DNS) with no direct fallback. Applies to all selected MCP
  connections when set. MCP errors withhold bodies, headers, trailers and redirects.

## Image contract

Users build images in their own CI; startup runs `ImageProbe` offline and fails closed.
Official bases are in [`images.txt`](images.txt); `examples/agent-image/debian.Dockerfile`
shows a glibc Debian build. Compatible images:

- match Docker platform (`linux/amd64` or `linux/arm64`) and declare no volumes;
- provide `/bin/sh` (`sleep`, `mkdir`, `cat`, `chmod`, `mv`, `tar`) and a `Supported`
  `opencode` (`image.go`); `/opt/multica-sandbox` is reserved;
- ship no `/etc/opencode`, `/opencode.json[c]`, `/.opencode`, or `OPENCODE_*`/`MULTICA_*` `ENV`;
- run as uid 65532 on read-only rootfs without network (`/workspace` tmpfs executable;
  `/tmp` `noexec`; with workdir volume, `HOME`, `XDG_CACHE_HOME`, `TMPDIR` and
  `XDG_DATA_HOME` live on it);
- carry no secrets. Entrypoint, command, `USER` and `HEALTHCHECK` are ignored.

**Resources, pool and Sysbox:** `resources` (`cpus`, `cpu_mode`, `memory`, `pids`,
`nofile`, `workspace_size`, `tmp_size`, `shm_size`) bound attempts and probes (defaults:
2, `4g`, 1024, 4096, `1g`, `256m`, `64m`; Sysbox raises `pids` 2048, `nofile` 65536,
`tmp_size` `512m`). `cpu_mode: shared` drops CPU quotas. `pool` shares a host cgroup
slice with PSI admission; `auto_admission: true` + `disk_path` adds cold-start pacing,
headroom and disk/inode watermarks (ADR 0020, [ADR 0028](../../adrs/0028-auto-host-resources.md)).
`runtime: sysbox-runc` enables nested Docker (`dockerd`, `containerd`, `runc`, `iptables`,
`helper`, `dind`; ADR 0019).

**Tool bundles & ENV:** `tools` mounts digest-pinned read-only bundles at
`/opt/multica-sandbox/tools/<name>` on `PATH` after `multica`, rejecting duplicates,
missing paths, reserved names and failing checks (ADR 0002; `examples/tool-bundle`).
Ordinary `custom_env` passes through; controller-owned vars (`DOCKER_*`, `MULTICA_*`,
`OPENCODE_*`, `XDG_*`, `GIT_*`, `GH_*`, `GITLAB_*`, `SSL_CERT_*`, `LD_*`, `HOME`, `PATH`,
proxies) and connection secrets (`INFERENCE_*`, `IAM_CLIENT_*`, MCP header ENV) are
excluded with a Sandbox Notice (never values). `custom_args` are ignored.

## Multica API relay

`multica_relay` (`listen`, `url`) relays `multica` CLI calls, swapping opaque `mat_relay_`
grants for in-memory `mat_` tokens (ADR 0014). `multica_cli` mounts the digest-pinned CLI
(`deploy/multica-cli.Dockerfile`) read-only first on `PATH` at `/opt/multica-sandbox/multica`.
Issue/chat tasks receive upstream prompts, `AGENTS.md`, `.multica/project/resources.json`,
`MULTICA_*` env, OpenCode tools (except `question`/`doom_loop`) and skills (ADR 0018).
Grants end at cleanup/restart; `/api/` uses native Multica authorization (ADR 0026).
With `helper`, `git_relay`, `git_file` and optional `forge_relay`, `multica repo checkout`,
`git push` and `gh`/`glab` work inside attempts ([`internal/repo`](../repo/README.md)).

## External authority adapter

`internal/attempt` POSTs v1 JSON to one HTTPS endpoint (no redirects/proxies, re-reads
bearer file, requires HTTP 204; optional, ADR 0009). `renew` sends controller, attempt,
server, workspace/agent/task IDs, `mcp_url`, SHA-256 `token_hash` and `expires_at` (no
raw JWT). `revoke`/`recover` must be idempotent; ended attempts cannot re-enroll and
fingerprints cannot move. MCP servers verify JWT identity/audience and active grants per
call, failing closed on state loss.

## Native reporting

Startup is silent in transcripts (`/start` and logs record lifecycle; first agent event
is `seq: 1`). `coder/acp-go-sdk` drives `opencode acp` (no client fs/terminal; permission
requests rejected), batching `tool_use`, `tool_result` (8 KiB preview) and text/reasoning
every 500 ms with idempotency keys (512-row retry buffer). `idle_watchdog` (`10m`) and
`tool_watchdog` (`2h`) bound silence; background shells hold the attempt until idle via
`opencode api --standalone`; memory stalls report OOM (ADR 0032). Error bodies are
withheld; redacted stderr tail (8 KiB) logs at `WARN` on failure. Usage reports once per
turn before completion.

With `sessions` and `helper`, issue/chat tasks retain one workdir volume per
(controller, workspace, agent, issue/chat) holding `OPENCODE_DB`. Follow-ups resume via
`session/load` (retiring lost sessions once), apply warm hints (`coalesced-comments-v1`)
and support web/channel chat prompts (ADRs 0017, 0026). Sweeps (`interval` 2h, `grace`
24h) remove done/cancelled issue volumes, prune artifacts/caches (`artifact_age` 12h),
enforce `budget` and reclaim under disk pressure ([ADR 0034](../../adrs/0034-session-disk-reclamation.md)).
Each attempt emits one `attempt finished` JSON log event (ADR 0025).

## Conformance

Supported path: persistent controller, Docker projected backend, native OpenCode.
Evidence is fixture-based, not production certification.

**Pins:** Multica v0.7.2 (`f05a525ac8e82f5e9169ad078e44f166fad6d5c8`); OpenCode 1.18.34,
1.18.35, 2.2.2, 2.2.5 (`images.txt`); ACP Go SDK 0.13.5; LiteLLM v1.104.0; Keycloak
26.8.0; MCP Go SDK 1.8.0; go-oidc 3.21.0; Docker Engine 29.2.1 (runc, cgroup v2,
`linux/arm64`; `amd64` unverified). Reproduce: `MULTICA_SOURCE=<checkout> VERIFY_CONTAINERS=1 VERIFY_SERVICE=1 VERIFY_OPENCODE=1 VERIFY_INFERENCE=1 VERIFY_IDENTITY=1 ./verify.sh`;
re-test a release with `OPENCODE_IMAGE=<image> ./scripts/test-opencode.sh`.

| Verified area | Evidence |
| --- | --- |
| Context, project resources, repo refs | `projection_test.go`, `project_test.go`, `projected_test.go`, `integration/{managed_mcp,project}_test.go` |
| ACP turns, quiet start, retries, cancel, usage, stderr | `acp_test.go`, `events_test.go`, `stderr_test.go`, `adapter_test.go`, `startup_test.go`, `outbox_test.go`, `integration/{adapter_reporting,quiet_start,retry,upstream}_test.go` |
| Task API, autopilot, chat, skills | `native_tasks_test.go`, `chat_test.go`, `skills_test.go`, `notices_test.go`, `skills_containers_test.go`, `integration/{task_authority,native_claims,multica_relay}_test.go` |
| Dynamic & managed MCP JWT rotation/leases | `public_mcp_test.go`, `mcp_selection_test.go`, `mcp_test.go`, `internal/identity`, `examples/identity-mcp/{rotation*,scenario}.go`, `scripts/test-dynamic-mcp.sh` |
| Multica relay, inference keys, secret isolation | `internal/{relay,inference}`, `policy_test.go`, `examples/identity-mcp/inference.go`, `integration/{multica_relay,inference,fleet,service_recovery}_test.go` |
| Images, tool bundles, official/Debian bases | `image_test.go`, `tools_containers_test.go`, `create_failure_test.go`, `examples/agent-image/debian.Dockerfile` |
| Repo checkout/push/PR, sessions & GC | `internal/repo`, `session_test.go`, `sessions_gc_test.go`, `{workdir,sessions}_test.go`, `integration/{issue_gc,multica_relay}_test.go` |
| Hostile isolation (caps, limits, net, DNS, IPv6) | `internal/docker/{backend,projected}_test.go` |

**Deployment-owned:** egress policy, attempt-network TLS, host/kernel hardening
(gVisor/Kata/Sysbox), workload attestation/authority, MCP authorization, gateway
budgets/routing. Revocation denies new MCP calls (relay streams cancel, in-flight MCP
work does not). No backend deadline exists while controller is down (#6).

**Limitations:** verified OpenCode releases only (`images.txt`, `scripts/opencode-release.sh`);
ACP usage/child-session details are observed behavior; unanswered turns may lose
in-flight step usage; stderr redaction is best-effort. Resume needs `sessions` + `helper`.
Unsupported: squad-leader variants, controller-pinned `local_directory`, `skill-bundles-v1`,
broker MCP, connected apps, artifact publication. Channel replies are text-only; Telegram
v0.7.2 lacks a lost-session history reader. Docker `local` volumes lack byte quotas;
image mounts need containerd image store and lack `nosuid`/`nodev` (re-run
`VERIFY_CONTAINERS=1` after Docker upgrades).

**Inspection sources:** Multica `server/internal/daemon/{types,client,prompt,gc}.go`,
`server/internal/handler/daemon.go`, `server/pkg/agent/opencode_acp.go`; OpenCode
`packages/opencode/src/acp/`.
