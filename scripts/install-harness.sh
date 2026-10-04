#!/bin/sh
# Pin the installer and CLI; the installer verifies the release archive checksum.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$ROOT"
INSTALLER=$(mktemp)
trap 'rm -f "$INSTALLER"' EXIT HUP INT TERM
curl -fsSL --retry 3 \
  https://raw.githubusercontent.com/gently-whitesnow/harness-cli/d3a529efceb17dd941743defe14929b2cd56aae4/install.sh \
  -o "$INSTALLER"
HARNESS_VERSION=3.9.2 sh "$INSTALLER" --scope clone
