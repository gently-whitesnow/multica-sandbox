# Agent guide

Build an independent execution boundary for Multica agents. The runtime is not
implemented yet; distinguish planned capabilities from verified behavior.

## Map

- `README.md`: purpose, scope and contributor setup.
- `adrs/`: short architectural decisions; start with ADR 0001.
- `.harness.json`: pinned repository quality contract.
- `scripts/install-harness.sh`, `verify.sh`: setup and verification.

## Rules

- Write code, documentation and commit messages in English. Be concise.
- Keep Multica as the task authority; do not introduce a second task queue.
- Keep credentials, runtime state and downloaded tools outside Git.
- Keep controller privileges outside agent execution environments.
- Record architectural changes in numbered ADRs. Keep navigation here.
- Add language checks and meaningful runtime tests when implementation starts.

## Verification

Install Harness once with `./scripts/install-harness.sh`. Stage new files before
running `./verify.sh`: Harness reads the Git index. Use `harness explain <id>`
through the clone-local binary to investigate findings; fix their cause rather
than weakening the frame. Inspect `git diff --check` before committing.
