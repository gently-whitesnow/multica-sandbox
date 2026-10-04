#!/bin/sh
# Verify the repository scaffold; runtime checks belong here once implemented.
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
sh -n scripts/install-harness.sh verify.sh
git diff --check
git diff --cached --check
