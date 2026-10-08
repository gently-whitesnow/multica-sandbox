#!/bin/sh
# Native OpenCode -> embedded relay -> real LiteLLM workspace keys; deterministic provider.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
PROJECT="sandbox-inference-$$"
TMP=$(mktemp -d)
compose() { docker compose -p "$PROJECT" --profile inference -f "$ROOT/examples/identity-mcp/compose.yaml" -f "$TMP/inference.yaml" "$@"; }
cleanup() { compose down -v --remove-orphans --rmi local; rm -rf "$TMP"; }
trap cleanup EXIT
cat > "$TMP/inference.yaml" <<EOF_CONFIG
services:
  keycloak:
    mem_limit: 512m
    environment:
      JAVA_OPTS_KC_HEAP: "-Xms64m -Xmx256m"
  gateway:
    environment:
      INFERENCE_FIXTURE: "1"
  inference:
    build:
      context: $ROOT
      dockerfile: examples/identity-mcp/Dockerfile
      target: rotation
    container_name: ${PROJECT}-relay
    command: [inference]
    profiles: [inference]
    networks:
      fixture: {}
      execution:
        aliases: [inference-relay]
    environment:
      EXECUTION_NETWORK: ${PROJECT}_execution
      RELAY_PEER: ${PROJECT}-relay
      LITELLM_DB: ${PROJECT}-litellm-db-1
    volumes:
      - credentials:/secrets:ro
      - /var/run/docker.sock:/var/run/docker.sock
networks:
  execution:
    internal: true
EOF_CONFIG
docker pull "$(grep '^ghcr' "$ROOT/internal/opencode/images.txt" | tail -n 1)" >/dev/null
compose build
compose up -d --wait --wait-timeout 240 litellm || { compose logs --no-color litellm 2>&1 | tail -20; exit 1; }
compose up --no-deps --no-log-prefix --abort-on-container-exit --exit-code-from inference inference
