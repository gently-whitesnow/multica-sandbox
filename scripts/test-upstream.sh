#!/bin/sh
# Disposable contract test; never points at an existing database or deployment.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$ROOT"
REV=$(sed -n 's/^const UpstreamRevision = "\([0-9a-f]\{40\}\)"$/\1/p' internal/multica/client.go)
[ -n "$REV" ] || { printf '%s\n' 'multica.UpstreamRevision not found' >&2; exit 2; }
REPO=${MULTICA_UPSTREAM_REPO:-https://github.com/multica-ai/multica}
TMP=$(mktemp -d)
NAME="multica-sandbox-contract-$$"
DB="$NAME-db"
SERVER="$NAME-server"
cleanup() {
 docker rm -fv "$SERVER" "$DB" >/dev/null 2>&1 || true
 docker network rm "$NAME" >/dev/null 2>&1 || true
 for part in controller agent cli tool-jq helper; do docker image rm "$NAME-$part:local" >/dev/null 2>&1 || true; done
 rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
# Builds use a disposable clone at exactly REV of MULTICA_SOURCE, else of the sibling
# checkout `multica repo checkout "$REPO"` creates beside this one, else a fetch of REPO.
SOURCE=${MULTICA_SOURCE:-}
SIBLING=${REPO%/}
SIBLING=$(basename "${SIBLING%.git}")
SIBLING=$ROOT/../${SIBLING##*:}
if [ -z "$SOURCE" ] && [ -e "$SIBLING/.git" ] && git -C "$SIBLING" cat-file -e "$REV^{commit}" 2>/dev/null; then
 SOURCE=$SIBLING
fi
UPSTREAM=$TMP/upstream
if [ -n "$SOURCE" ]; then
 git -C "$SOURCE" cat-file -e "$REV^{commit}" 2>/dev/null || { printf '%s lacks %s; fetch it or unset MULTICA_SOURCE\n' "$SOURCE" "$REV" >&2; exit 2; }
 git clone -q --shared --no-checkout -- "$SOURCE" "$UPSTREAM"
else
 git init -q "$UPSTREAM"
 git -C "$UPSTREAM" fetch -q --depth=1 "$REPO" "$REV"
fi
git -C "$UPSTREAM" -c advice.detachedHead=false checkout -q --detach "$REV"
[ "$(git -C "$UPSTREAM" rev-parse HEAD)" = "$REV" ]
git -C "$UPSTREAM" diff --quiet HEAD --
printf 'Upstream %s from %s\n' "$REV" "${SOURCE:-$REPO}"
ARCH=$(docker info --format '{{.Architecture}}')
case "$ARCH" in aarch64|arm64) ARCH=arm64 ;; x86_64|amd64) ARCH=amd64 ;; *) exit 1 ;; esac
(cd "$UPSTREAM/server" && go build -o "$TMP/migrate" ./cmd/migrate && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$TMP/server" ./cmd/server)
docker network create "$NAME" >/dev/null
POSTGRES_PASSWORD=$(openssl rand -hex 24)
export POSTGRES_PASSWORD
JWT_SECRET=$(openssl rand -hex 32)
export JWT_SECRET
docker run -d --name "$DB" --network "$NAME" -p 127.0.0.1::5432 -e POSTGRES_PASSWORD -e POSTGRES_USER=contract -e POSTGRES_DB=contract pgvector/pgvector@sha256:cf134a767f474095eeba57e0117be8e568e011a63f33fbf252f14c9b760f8e6f >/dev/null
DBPORT=$(docker port "$DB" 5432/tcp | cut -d: -f2)
(cd "$UPSTREAM/server" && DATABASE_URL="postgres://contract:$POSTGRES_PASSWORD@127.0.0.1:$DBPORT/contract?sslmode=disable" "$TMP/migrate" up) >"$TMP/migration.log" 2>&1 || { tail -20 "$TMP/migration.log"; exit 1; }
DATABASE_URL="postgres://contract:$POSTGRES_PASSWORD@$DB:5432/contract?sslmode=disable"
export DATABASE_URL
docker run -d --name "$SERVER" --network "$NAME" -p 127.0.0.1::8080 -e DATABASE_URL -e JWT_SECRET -e APP_ENV=development -e PORT=8080 -v "$TMP/server:/server:ro" alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc /server >/dev/null
PORT=$(docker port "$SERVER" 8080/tcp | cut -d: -f2)
MULTICA_TEST_URL="http://127.0.0.1:$PORT"
export MULTICA_TEST_URL
ATTEMPT=0
until curl -fsS "$MULTICA_TEST_URL/health" >/dev/null 2>&1; do
 ATTEMPT=$((ATTEMPT + 1))
 if [ "$ATTEMPT" -ge 60 ]; then docker logs --tail 20 "$SERVER"; exit 1; fi
 sleep 1
done
if [ "${VERIFY_SERVICE:-0}" = 1 ]; then
 docker build -t "$NAME-controller:local" .
 CONTROLLER=$(docker image inspect "$NAME-controller:local" --format '{{index .RepoDigests 0}}')
 test -n "$CONTROLLER"
 MULTICA_TEST_CONTROLLER_COMPOSE="$TMP/controller-image.yaml"
 printf 'services:\n  controller:\n    image: %s\n' "$CONTROLLER" > "$MULTICA_TEST_CONTROLLER_COMPOSE"
 export MULTICA_TEST_CONTROLLER_COMPOSE
fi
if [ "${VERIFY_SERVICE:-0}" = 1 ] && [ "${VERIFY_OPENCODE:-0}" = 1 ]; then
 mkdir "$TMP/agent"
 docker build -q -t "$NAME-agent:local" -f examples/agent-image/debian.Dockerfile "$TMP/agent" >/dev/null
 docker build -q -t "$NAME-cli:local" -f deploy/multica-cli.Dockerfile --build-context multica="$UPSTREAM" "$TMP/agent" >/dev/null
 docker build -q -t "$NAME-tool-jq:local" -f examples/tool-bundle/Dockerfile "$TMP/agent" >/dev/null
 docker build -q -t "$NAME-helper:local" --target helper . >/dev/null
 for IMAGE in $(grep '^ghcr' internal/opencode/images.txt); do docker pull -q "$IMAGE" >/dev/null; done
 # Digest references need the containerd image store; the backend accepts only pinned images.
 MULTICA_TEST_AGENT_IMAGE=$(docker image inspect "$NAME-agent:local" --format '{{index .RepoDigests 0}}')
 MULTICA_TEST_CLI_IMAGE=$(docker image inspect "$NAME-cli:local" --format '{{index .RepoDigests 0}}')
 MULTICA_TEST_TOOL_IMAGE=$(docker image inspect "$NAME-tool-jq:local" --format '{{index .RepoDigests 0}}')
 MULTICA_TEST_HELPER_IMAGE=$(docker image inspect "$NAME-helper:local" --format '{{index .RepoDigests 0}}')
 export MULTICA_TEST_AGENT_IMAGE MULTICA_TEST_CLI_IMAGE MULTICA_TEST_TOOL_IMAGE MULTICA_TEST_HELPER_IMAGE
fi
MULTICA_TEST_SERVER_CONTAINER="$SERVER" MULTICA_TEST_DB_CONTAINER="$DB" go test -tags=upstream -run "${UPSTREAM_TEST_FILTER:-.}" -count=1 -v ./integration
printf 'Verified upstream %s\n' "$REV"
