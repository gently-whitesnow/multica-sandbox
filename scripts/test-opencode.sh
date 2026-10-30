#!/bin/sh
# Disposable real-JWT/OpenCode fixture; inference is deterministic and credential-free.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
PROJECT="sandbox-opencode-$$"
TMP=$(mktemp -d)
# OPENCODE_IMAGE selects a listed image or a candidate release; the default is the newest listed.
IMAGE=${OPENCODE_IMAGE:-$(grep '^ghcr' "$ROOT/internal/opencode/images.txt" | tail -n 1)}
compose() { docker compose -p "$PROJECT" -f "$ROOT/examples/identity-mcp/compose.yaml" -f "$TMP/rotation.yaml" "$@"; }
cleanup() { compose down -v --remove-orphans --rmi local; rm -rf "$TMP"; }
trap cleanup EXIT
cat > "$TMP/rotation.yaml" <<EOF_CONFIG
services:
  seed:
    environment:
      ROTATION_FIXTURE: "1"
  provider:
    build:
      context: $ROOT
      dockerfile: examples/identity-mcp/Dockerfile
      target: fixture
    command: [provider]
    networks: [execution]
    healthcheck:
      test: [CMD, /identity-example, health]
      interval: 2s
      timeout: 2s
      retries: 60
  rotation:
    build:
      context: $ROOT
      dockerfile: examples/identity-mcp/Dockerfile
      target: rotation
    container_name: ${PROJECT}-relay
    command: [rotation]
    networks:
      fixture: {}
      execution:
        aliases: [mcp-relay]
    environment:
      EXECUTION_NETWORK: ${PROJECT}_execution
      PROVIDER_PEER: ${PROJECT}-provider-1
      RELAY_PEER: ${PROJECT}-relay
      OPENCODE_IMAGE: $IMAGE
    volumes:
      - credentials:/secrets:ro
      - /var/run/docker.sock:/var/run/docker.sock
networks:
  execution:
    internal: true
EOF_CONFIG
docker pull "$IMAGE" >/dev/null
compose build
compose up -d --wait --wait-timeout 180 gateway provider
compose up --no-deps --no-log-prefix --abort-on-container-exit --exit-code-from rotation rotation || { compose logs --no-color gateway 2>&1 | tail -40; exit 1; }
