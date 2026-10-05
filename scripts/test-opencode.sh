#!/bin/sh
# Disposable real-JWT/OpenCode fixture; inference is deterministic and credential-free.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
PROJECT="sandbox-opencode-$$"
TMP=$(mktemp -d)
compose() { docker compose -p "$PROJECT" -f "$ROOT/examples/identity-mcp/compose.yaml" -f "$TMP/rotation.yaml" "$@"; }
cleanup() { compose down -v --remove-orphans; rm -rf "$TMP"; }
trap cleanup EXIT
cat > "$TMP/rotation.yaml" <<EOF_CONFIG
services:
  seed:
    environment:
      ROTATION_FIXTURE: "1"
  gateway:
    networks: [fixture, execution]
  rotation:
    build:
      context: $ROOT
      dockerfile: examples/identity-mcp/Dockerfile
      target: rotation
    command: [rotation]
    networks: [fixture]
    environment:
      EXECUTION_NETWORK: ${PROJECT}_execution
      MCP_PEER: ${PROJECT}-gateway-1
    volumes:
      - credentials:/secrets:ro
      - /var/run/docker.sock:/var/run/docker.sock
networks:
  execution:
    internal: true
EOF_CONFIG
docker pull ghcr.io/anomalyco/opencode:1.18.34@sha256:b34342987ca889fc2cc19cbc046eefc2418e5980a3d696e209fbb401a288f631 >/dev/null
compose build
compose up -d --wait --wait-timeout 180 gateway
compose run --rm --no-deps rotation
