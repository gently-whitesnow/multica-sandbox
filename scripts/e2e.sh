#!/bin/sh
# Persistent local lab; subscription credentials stay in its named volume.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
compose() { docker compose -f "$ROOT/examples/end-to-end/compose.yaml" "$@"; }
case "${1:-}" in
 prepare)
  compose build controller agent-image multica
  compose up -d --wait --wait-timeout 180 multica keycloak mcp cli-proxy litellm frontend
  compose run --rm --no-deps controller setup
  ;;
 models) compose run --rm --no-deps controller models ;;
 login) compose run --rm --no-deps login ;;
 check) compose run --rm --no-deps controller check ;;
 run) compose run --rm --no-deps --use-aliases controller run ;;
 stop)
  compose run --rm --no-deps controller clean
  compose down
  ;;
 reset)
  compose run --rm --no-deps controller clean
  compose down
  docker volume rm sandbox-e2e_config sandbox-e2e_realm sandbox-e2e_database
  ;;
 *) printf '%s\n' 'Usage: ./scripts/e2e.sh prepare|login|models|check|run|stop|reset' >&2; exit 2 ;;
esac
