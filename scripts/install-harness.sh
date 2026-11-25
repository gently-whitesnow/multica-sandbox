#!/bin/sh
# Pin the installer and CLI; the installer verifies the release archive checksum.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$ROOT"
INSTALLER=$(mktemp)
trap 'rm -f "$INSTALLER"' EXIT HUP INT TERM
curl -fsSL --retry 3 \
  https://raw.githubusercontent.com/gently-whitesnow/harness-cli/707fcd489838cfd812e7abcd3e0c16e4e43e2649/install.sh \
  -o "$INSTALLER"
HARNESS_VERSION=4.0.0 sh "$INSTALLER" --scope clone
