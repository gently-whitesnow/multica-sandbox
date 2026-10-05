#!/bin/sh
# Native OpenCode + real Keycloak/LiteLLM, deterministic provider behind the gateway.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
PROJECT="sandbox-inference-$$"
TMP=$(mktemp -d)
compose() { docker compose -p "$PROJECT" -f "$ROOT/examples/identity-mcp/compose.yaml" -f "$TMP/inference.yaml" "$@"; }
cleanup() { compose down -v --remove-orphans; rm -rf "$TMP"; }
trap cleanup EXIT
cat > "$TMP/inference.yaml" <<EOF_CONFIG
services:
  seed:
    environment:
      ROTATION_FIXTURE: "1"
      INFERENCE_FIXTURE: "1"
  keycloak:
    mem_limit: 512m
    environment:
      JAVA_OPTS_KC_HEAP: "-Xms64m -Xmx256m"
  gateway:
    environment:
      INFERENCE_FIXTURE: "1"
    networks: [fixture, execution]
  litellm:
    mem_limit: 768m
    cpus: 0.5
    image: ghcr.io/berriai/litellm:v1.104.0@sha256:625981c83410a3ea68eb0697590a57ec1d764d634514d54fa5db0591077ee839
    networks: [fixture, execution]
    depends_on:
      gateway:
        condition: service_healthy
    entrypoint: [/bin/sh, -c, 'export LITELLM_MASTER_KEY="\$\$(cat /secrets/admin)"; export FIXTURE_UPSTREAM_KEY="\$\$(cat /secrets/upstream)"; exec litellm --config /fixture/litellm.yaml --port 4000']
    environment:
      PYTHONPATH: /fixture
      LITELLM_LOCAL_MODEL_COST_MAP: "True"
    volumes:
      - credentials:/secrets:ro
      - $ROOT/examples/identity-mcp/litellm.yaml:/fixture/litellm.yaml:ro
      - $ROOT/examples/identity-mcp/litellm_auth.py:/fixture/litellm_auth.py:ro
    healthcheck:
      test: [CMD, python, -c, "import urllib.request; urllib.request.urlopen('http://localhost:4000/health/liveliness')"]
      interval: 2s
      timeout: 2s
      retries: 60
  rotation:
    build:
      context: $ROOT
      dockerfile: examples/identity-mcp/Dockerfile
      target: rotation
    command: [${INFERENCE_SCENARIO:-inference-rotation}]
    networks: [fixture]
    environment:
      INFERENCE_FIXTURE: "1"
      EXECUTION_NETWORK: ${PROJECT}_execution
      MCP_PEER: ${PROJECT}-gateway-1
      INFERENCE_PEER: ${PROJECT}-litellm-1
    volumes:
      - credentials:/secrets:ro
      - /var/run/docker.sock:/var/run/docker.sock
networks:
  execution:
    internal: true
EOF_CONFIG
docker pull ghcr.io/anomalyco/opencode:1.18.34@sha256:b34342987ca889fc2cc19cbc046eefc2418e5980a3d696e209fbb401a288f631 >/dev/null
compose build
compose up -d --wait --wait-timeout 180 litellm || { compose logs --no-color litellm 2>&1 | rg "Error:|ModuleNotFoundError|ImportError|SyntaxError|Exception:"; exit 1; }
compose run --rm --no-deps rotation
