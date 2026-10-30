#!/bin/sh
# Admit an OpenCode release (ADR 0023): pin its multi-platform index, print the ACP spike's
# capability report, gate on the MCP rotation conformance and append it to images.txt for review.
# Usage: scripts/opencode-release.sh VERSION (for example 2.2.2). Requires Docker.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
VERSION=${1:?usage: opencode-release.sh VERSION}
printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "VERSION must be X.Y.Z" >&2; exit 2; }
REPOSITORY=ghcr.io/anomalyco/opencode
LIST="$ROOT/internal/opencode/images.txt"
if grep -q "^$REPOSITORY:$VERSION@" "$LIST"; then
  echo "$VERSION is already listed" >&2
  exit 0
fi
DIGEST=$(docker buildx imagetools inspect "$REPOSITORY:$VERSION" --format '{{json .Manifest.Digest}}' | tr -d '"')
printf '%s' "$DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$' || { echo "no index digest for $VERSION" >&2; exit 1; }
IMAGE="$REPOSITORY:$VERSION@$DIGEST"
docker pull "$IMAGE" >/dev/null
(cd "$ROOT" && go run ./examples/acp-spike -image "$IMAGE")
OPENCODE_IMAGE="$IMAGE" "$ROOT/scripts/test-opencode.sh"
printf '%s\n' "$IMAGE" >> "$LIST"
echo "appended $IMAGE to internal/opencode/images.txt; run the full conformance before merging"
