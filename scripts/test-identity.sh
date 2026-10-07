#!/bin/sh
# Run only the disposable identity fixture; remove its own volumes and images afterwards.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
PROJECT="sandbox-identity-$$"
compose() { docker compose -p "$PROJECT" -f "$ROOT/examples/identity-mcp/compose.yaml" "$@"; }
trap 'compose down -v --remove-orphans --rmi local' EXIT
compose build
compose up -d --wait --wait-timeout 180 gateway
compose run --rm --no-deps scenario
