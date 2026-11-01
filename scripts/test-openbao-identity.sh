#!/bin/sh
# Disposable loopback-only OpenBao issuer; no existing host or production secrets.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$ROOT"
CONTAINER="sandbox-openbao-identity-$$"
SECRET_DIR=$(mktemp -d)
cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$SECRET_DIR"
}
trap cleanup EXIT HUP INT TERM
openssl rand -hex 32 | tr -d '\n' > "$SECRET_DIR/root"
chmod 600 "$SECRET_DIR/root"
BAO_DEV_ROOT_TOKEN_ID=$(cat "$SECRET_DIR/root")
export BAO_DEV_ROOT_TOKEN_ID
docker run --detach --name "$CONTAINER" --rm --publish 127.0.0.1::8200 \
  --env BAO_DEV_ROOT_TOKEN_ID openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641 \
  server -dev -dev-listen-address=0.0.0.0:8200 >/dev/null
unset BAO_DEV_ROOT_TOKEN_ID
PORT=$(docker port "$CONTAINER" 8200/tcp | sed 's/127.0.0.1://')
SANDBOX_TEST_BAO_URL="http://127.0.0.1:$PORT"
export SANDBOX_TEST_BAO_URL
READY=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if curl --fail --silent "$SANDBOX_TEST_BAO_URL/v1/sys/health" >/dev/null; then READY=1; break; fi
  sleep 1
done
[ "$READY" = 1 ]
SANDBOX_TEST_BAO_ROOT_FILE="$SECRET_DIR/root" go test -race ./internal/identity -run '^TestDisposableOpenBao$' -v
