# Agent guide

Build an independent execution boundary for Multica agents. An experimental offline
Docker executor and fake lifecycle probe exist. Distinguish planned capabilities
from verified behavior.

## Map

- `README.md`: purpose, scope and contributor setup.
- `adrs/`: architecture (0001), images/tools (0002), identity/MCP (0003),
  component reuse and security acceptance gates (0004), lifecycle probe (0005),
  offline execution and recovery (0006), containerized service (0007),
  multi-workspace authority (0008), attempt authorization (0009), external services/adapters (0010),
  managed MCP identity delivery (0011), inference credential translation (0012),
  identity resolution contract (0013).
- `examples/end-to-end/`: opt-in real Multica/OpenCode/inference lab.
- `examples/identity-mcp/`: disposable identity and MCP contract fixture.
- `internal/identity/`: static/external identity resolution and verified Keycloak issuance.
- `cmd/sandbox-controller`, `internal/service/`: persistent controller service.
- `Dockerfile`, `compose.yaml`, `deploy/`: controller packaging/configuration.
- `cmd/sandbox-probe`, `internal/`: test executor, controller and HTTP integration.
- `internal/docker/`, `internal/execution/`: offline backend and execution contract.
- `integration/`, `scripts/test-upstream.sh`: disposable upstream contract tests.
- [GitHub Project](https://github.com/users/gently-whitesnow/projects/2): delivery;
  issues hold acceptance and handoff state, ADRs hold durable decisions.
- `.harness.json`: pinned repository quality contract.
- `scripts/install-harness.sh`, `verify.sh`: setup and verification.

## Rules

- Keep code and documentation concise.
- Separate sandbox backends, agent adapters and inference configuration.
- Keep custom images and declarative tool sets first-class configuration.
- Keep Multica as the task authority; do not introduce a second task queue.
- Keep credentials, runtime state and downloaded tools outside Git.
- Keep controller privileges and service credentials outside agent environments.
- Allow only short-lived run identity inside; authorize external actions through
  trusted MCP services and enforce network policy outside the sandbox.
- Prefer maintained components. Document a concrete gap before replacing one.
- Require adversarial conformance checks before advertising backend or adapter
  support; a working happy path is insufficient.
- Record architectural changes in numbered ADRs. Keep navigation here.
- On Harness upgrades, review new checks and require every applicable check.
- Add language checks and meaningful runtime tests as the implementation grows.

## Verification

Install Harness once with `./scripts/install-harness.sh`. Stage new files before
running `./verify.sh`: Harness reads the Git index. Use `harness explain <id>`
through the clone-local binary to investigate findings; fix their cause rather
than weakening the frame. Inspect `git diff --check` before committing.
Run `VERIFY_UPSTREAM=1 ./verify.sh` for lifecycle/client changes; Docker must be
available. Run `VERIFY_CONTAINERS=1 ./verify.sh` for backend changes and
`VERIFY_SERVICE=1 ./verify.sh` for service/packaging changes. Never run test commands
against production tasks. Keep fixture data generic.
Run `VERIFY_IDENTITY=1 ./verify.sh` for identity/MCP fixture changes.
The end-to-end lab uses `scripts/e2e.sh`; real inference requires explicit local
subscription login. Never copy account data into documentation or test reports.
CI is paused; run verification locally. Do not re-enable CI without an explicit
request. Use `VERIFY_COMMIT_RANGE=master..HEAD ./verify.sh` before publishing.
Before starting an issue, inspect its ADRs and existing branches/PRs. Leave a concise
handoff with commit, verified result, next action and blocker when interrupted.
