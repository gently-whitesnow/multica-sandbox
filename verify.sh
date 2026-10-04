#!/bin/sh
# Verify the probe and repository contract.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
cd "$ROOT"
COMMON=$(git rev-parse --git-common-dir)
HARNESS="$COMMON/harness/bin/harness"
if [ ! -x "$HARNESS" ]; then
  printf '%s\n' 'Run ./scripts/install-harness.sh first.' >&2
  exit 2
fi
"$HARNESS" check
for script in scripts/*.sh verify.sh; do sh -n "$script"; done
if [ -n "${VERIFY_COMMIT_RANGE:-}" ]; then
  "$HARNESS" commits check "$VERIFY_COMMIT_RANGE"
fi
[ -z "$(gofmt -l cmd internal integration examples)" ]
go vet ./...
go test -race ./...
go build ./...
if [ "${VERIFY_CONTAINERS:-0}" = 1 ]; then
 go test -race -tags containers -count=1 ./internal/docker
fi
if [ "${VERIFY_UPSTREAM:-0}" = 1 ] || [ "${VERIFY_SERVICE:-0}" = 1 ]; then ./scripts/test-upstream.sh; fi
if [ "${VERIFY_IDENTITY:-0}" = 1 ]; then ./scripts/test-identity.sh; fi
git diff --check
git diff --cached --check
