# Agent guide

Build an independent execution boundary for Multica agents. The runtime is not
implemented yet; a fake lifecycle probe exists. Distinguish planned capabilities
from verified behavior.

## Map

- `README.md`: purpose, scope and contributor setup.
- `adrs/`: architecture (0001), images/tools (0002), identity/MCP (0003),
  component reuse and security acceptance gates (0004), lifecycle probe (0005).
- `cmd/sandbox-probe`, `internal/`: test executor, controller and HTTP integration.
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
available. Never run the probe against production tasks. Keep fixture data generic.
CI is paused; run verification locally. Do not re-enable CI without an explicit
request. Use `VERIFY_COMMIT_RANGE=master..HEAD ./verify.sh` before publishing.
Before starting an issue, inspect its ADRs and existing branches/PRs. Leave a concise
handoff with commit, verified result, next action and blocker when interrupted.
