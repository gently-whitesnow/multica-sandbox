#!/bin/sh
# Native OpenCode and SDK MCP over an isolated synthetic public proxy; no external account.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
PROJECT="sandbox-dynamic-mcp-$$"
TMP=$(mktemp -d)
IMAGE=${OPENCODE_IMAGE:-$(grep '^ghcr' "$ROOT/internal/opencode/images.txt" | tail -n 1)}
HELPER="$PROJECT-helper:local"
compose() { docker compose -p "$PROJECT" -f "$TMP/compose.yaml" "$@"; }
cleanup() { compose down -v --remove-orphans --rmi local; docker image rm "$HELPER" >/dev/null 2>&1 || true; rm -rf "$TMP"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
docker pull "$IMAGE" >/dev/null
docker build -q --target helper -t "$HELPER" "$ROOT" >/dev/null
MCP_HELPER=$(docker image inspect "$HELPER" --format '{{index .RepoDigests 0}}')
test -n "$MCP_HELPER"
cat > "$TMP/compose.yaml" <<EOF_CONFIG
services:
  dynamic:
    build:
      context: $ROOT
      dockerfile: examples/identity-mcp/Dockerfile
      target: rotation
    container_name: ${PROJECT}-relay
    command: [dynamic-mcp]
    networks:
      execution:
        aliases: [mcp-relay, provider]
    environment:
      EXECUTION_NETWORK: ${PROJECT}_execution
      RELAY_PEER: ${PROJECT}-relay
      OPENCODE_IMAGE: $IMAGE
      MCP_HELPER_IMAGE: $MCP_HELPER
    volumes: [/var/run/docker.sock:/var/run/docker.sock]
networks:
  execution:
    internal: true
EOF_CONFIG
compose build
compose up --no-log-prefix --abort-on-container-exit --exit-code-from dynamic dynamic
