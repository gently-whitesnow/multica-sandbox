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
sh -n scripts/*.sh verify.sh
[ -z "$(gofmt -l cmd internal integration)" ]
go vet ./...
go test -race ./...
go build ./...
if [ "${VERIFY_UPSTREAM:-0}" = 1 ]; then ./scripts/test-upstream.sh; fi
git diff --check
git diff --cached --check
