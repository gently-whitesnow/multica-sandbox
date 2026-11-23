# Agent guide

Build an independent execution boundary for Multica agents. An experimental offline
Docker executor and fake lifecycle probe exist. Distinguish planned capabilities
from verified behavior.

## Map

- `README.md`: purpose, scope and contributor setup.
- `adrs/`: architecture (0001), images/tools (0002), identity/MCP (0003),
  component reuse/security gates (0004), lifecycle probe (0005), offline execution/recovery (0006),
  containerized service (0007), multi-workspace authority (0008), attempt authorization (0009),
  external services/adapters (0010), managed MCP identity (0011), inference relay (0012),
  identity resolution (0013), Multica API relay (0014), repository workdirs/Git/sessions (0015),
  `gh pr create` via remote aliases (0016), web chat tasks (0017), agent skills (0018),
  nested Docker under sysbox-runc (0019), shared attempt memory pool (0020),
  daemon WebSocket wakeups (0021), inference/identity from Multica env (0022),
  OpenCode ACP and MCP relay (0023), in-house credential relay (0024),
  metrics and structured logs (0025), Multica 0.7.2 and task capabilities (0026),
  provider-neutral JWT identity (0027), auto host resources (0028), dynamic HTTP MCP (0029),
  native env and explicit IAM (0030), controller self-deploy (0031),
  OpenCode background shells and watchdogs (0032), release versions (0033),
  session disk reclamation (0034), reference mirrors (0035).
- `examples/end-to-end/`: opt-in real Multica/OpenCode/inference lab.
- `examples/identity-mcp/`: disposable identity and MCP contract fixture.
- `examples/agent-image/`: example user-owned OpenCode Debian image (`deploy/multica-cli.Dockerfile` provides `multica`).
- `examples/tool-bundle/`: example user-owned tool bundle (static `jq`).
- `examples/acp-spike/`: `opencode acp` capability spike with fixtures (ADR 0023).
- `examples/sysbox-host/`: Lima/Linux Sysbox host recipe, attempt pool slice and engine test image (ADR 0019, 0020).
- `internal/inference/`: workspace gateway bindings, relay policy and advisory model catalogs.
- `internal/identity/`: static/external identity resolution and verified Keycloak issuance.
- `internal/relay/`: shared credential-translation reverse proxy and per-attempt grants (Multica, inference, Git, forge).
- `internal/baseurl/`: base-path validation for server and gateway URLs behind a prefix.
- `internal/repo/`, `cmd/sandbox-helper`: repository checkout, Git/forge relays and attempt helper (workdir volume, loopback forwarder, nested engine supervisor).
- `internal/pool/`: attempt pool slice limits, PSI admission and attempt memory events (ADR 0020).
- `internal/metrics/`: controller metrics on `/metrics` (ADR 0025).
- `internal/opencode/`: OpenCode ACP adapter, MCP relay, native reporting, conformance and verified official base images (`images.txt`).
- `cmd/sandbox-controller`, `internal/service/`: persistent controller service.
- `Dockerfile`, `compose.yaml`, `deploy/`: controller packaging and configuration.
- `cmd/sandbox-probe`, `internal/`: test executor, controller and HTTP integration.
- `internal/docker/`, `internal/execution/`: offline backend and execution contract.
- `integration/`, `scripts/test-upstream.sh`: disposable upstream contract tests.
- [GitHub Project](https://github.com/users/gently-whitesnow/projects/2): delivery; issues hold acceptance/handoff state, ADRs hold durable decisions.
- `.harness.json`: pinned repository quality contract.
- `scripts/install-harness.sh`, `verify.sh`, `scripts/opencode-release.sh`: setup, verification and OpenCode release admission (ADR 0023).

## Rules

- Keep code and documentation concise.
- Remove superseded prototype paths unless a deployed consumer requires compatibility.
- Separate sandbox backends, agent adapters and inference configuration.
- Keep custom images and declarative tool sets first-class configuration.
- Keep Multica as the task authority; do not introduce a second task queue.
- Keep credentials, runtime state and downloaded tools outside Git.
- Keep controller privileges and service credentials outside agent environments.
- Allow only short-lived run identity inside; authorize external actions through trusted MCP services and enforce network policy outside the sandbox.
- Prefer maintained components. Document a concrete gap before replacing one.
- Require adversarial conformance checks before advertising backend or adapter support; a happy path is insufficient.
- Keep ADRs focused on distinct decisions; update existing ones for prototype refinements. Record architectural changes in numbered ADRs and keep navigation here.
- On Harness upgrades, review new checks and require every applicable check.
- Add language checks and meaningful runtime tests as the implementation grows.
- Bump `VERSION` in every PR that changes a published image or its configuration (ADR 0033).

## Verification

Install Harness once with `./scripts/install-harness.sh`. Stage new files before
running `./verify.sh`: Harness reads the Git index. Use `harness explain <id>`
via the clone-local binary to investigate findings; fix their cause rather than
weakening the frame. Inspect `git diff --check` before committing.

Verification flags (Docker required for container/integration suites; keep fixture data generic and never run against production tasks):
- `VERIFY_UPSTREAM=1 ./verify.sh`: lifecycle/client changes. Builds `multica.UpstreamRevision` from `MULTICA_SOURCE`, else a sibling `multica repo checkout <repo>` holding that revision, else fetches `MULTICA_UPSTREAM_REPO` (default `https://github.com/multica-ai/multica`). Note: `docker ... signal: killed` is usually an `exec.CommandContext` deadline; check `memory.events` for `oom_kill` before suspecting OOM.
- `VERIFY_CONTAINERS=1 ./verify.sh`: backend changes.
- `VERIFY_SERVICE=1 ./verify.sh`: service/packaging changes.
- `VERIFY_IDENTITY=1 ./verify.sh`: identity/MCP fixture changes.
- `VERIFY_DYNAMIC_MCP=1 ./verify.sh`: public remote MCP selection/transport (real OpenCode tools, secret isolation, retained changes).
- `VERIFY_INFERENCE=1 ./verify.sh`: native OpenCode/relay/LiteLLM workspace-key checks; add `VERIFY_SERVICE=1 VERIFY_OPENCODE=1` for real controller inference.
- `VERIFY_COMMIT_RANGE=master..HEAD ./verify.sh`: validate commits before publishing.
- `scripts/e2e.sh`: end-to-end lab (real inference requires explicit local subscription login; never copy account data into docs or reports).

CI is paused; run verification locally and do not re-enable CI without an explicit request.
`images.yml` (manual) publishes controller, helper and CLI images for one platform;
`deploy.yml` rolls out the controller. Before starting an issue, inspect its ADRs and
existing branches/PRs. Leave a concise handoff (commit, verified result, next action,
blocker) when interrupted.

## Controller release

You run inside this controller, so release it through `deploy.yml` (ADR 0031,
vps-worker ADR-0024), never through vps-worker Ansible. It changes only the
controller image; helper, CLI, agent image, secrets, policy and network changes
require a vps-worker infrastructure window with an idle queue.

1. Use a reviewed commit on `master`. Verify `actions/runs?head_sha=<SHA>` has a
   successful `images.yml` run for `platform=linux/amd64`; otherwise dispatch it once
   (`{"ref":"master","inputs":{"platform":"linux/amd64"}}`). Never dispatch `deploy.yml`
   while another `deploy.yml` run is queued or in progress.
2. Save continuation first, because the drain waits for your run and restart follows:
   `multica issue wakeup create <issue> --agent-id <your-agent-uuid> --kind at --after 40m --instruction-file ./resume-delivery.md`
   (at least `drain_minutes` + 10). Record SHA, expected digest from `images.yml`, dispatch time and step 4.
3. Dispatch and end the run, leaving the issue `in_progress`:
   `printf '%s\n' '{"ref":"master","inputs":{"sha":"<SHA>","drain_minutes":"30"}}' | gh api --method POST /repos/gently-whitesnow/multica-sandbox/actions/workflows/deploy.yml/dispatches --input -`.
   Claims stop, running attempts finish, then only the controller restarts; new tasks wait in Multica.
   A drain timeout cancels the release (`drain_timeout`). Set `force:true` only on explicit Fred/owner
   decision: interrupted attempts get `runtime_recovery` (one resumed retry within budget, else visible
   failure; autopilot runs are not retried).
4. After wakeup, check `actions/workflows/deploy.yml/runs?per_page=5`: matching `head_sha`,
   conclusion, summary `success`, `running` matching expected digest, and `/readyz 200, stable`.
   `rolled_back` means failed release (diagnose from logs); `unchanged` means digest already ran.
   Verify the change in this attempt and record run ID, SHA and digest in the issue.
5. Roll back by deploying the previous `master` SHA the same way. Missing host `RESULT` or failed
   rollback is a blocker for Fred with the run ID; never request root or bypass host capability.
