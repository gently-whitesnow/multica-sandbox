#!/bin/bash
# Smoke of nested Docker under the attempt policy (ADR 0019): user namespace, docker:dind,
# then the sandbox createArgs with sysbox-runc (read-only root, cap-drop=ALL,
# no-new-privileges, PID 1 root plus exec as 65532, internal network with a pull-through
# mirror peer), where inner data lives, cleanup and SYSBOX_HONOR_CAPS.
#   examples/sysbox-host/smoke.sh     resources labelled sbx-smoke are removed afterwards
# Runs only against the Sysbox context (default lima-sysbox), never the default engine.
set -uo pipefail
export DOCKER_CONTEXT=${DOCKER_CONTEXT:-lima-sysbox}
VM=${LIMA_SYSBOX_NAME:-sysbox}
ALPINE=alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
DIND=docker@sha256:e8faad5a8dc5279dff929afc5449f2791736912fff9f99351d742db2fad01b4c # 29.7.1-dind
REGISTRY=registry@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8 # registry:3
L=sbx-smoke
P=sbx-smoke
rc=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; rc=1; }
info() { echo "INFO  $*"; }
vm() { limactl shell "$VM" -- sudo "$@"; }

cleanup() {
  ids=$(docker ps -aq --filter "label=$L")
  # shellcheck disable=SC2086 # container ids
  [ -n "$ids" ] && docker rm -fv $ids >/dev/null
  docker network rm "$P-exec" "$P-up" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup
SMOKE_IMAGE=sbx-smoke-dind:local
here=$(cd "$(dirname "$0")" && pwd)
docker build -q -f "$here/smoke.Dockerfile" -t $SMOKE_IMAGE "$here" >/dev/null || { echo "smoke image build failed" >&2; exit 1; }

# Inner dockerd readiness: from State.StartedAt to the first successful `docker info` inside.
wait_dockerd() { # name [user] (docker:dind with its own entrypoint)
  docker exec ${2:+--user "$2"} "$1" sh -c 'i=0; until docker -H unix:///run/docker.sock info >/dev/null 2>&1; do i=$((i+1)); [ $i -gt 600 ] && exit 1; sleep 0.05; done; date +%s.%N' 2>/dev/null
}
ready_seconds() { # name ready_epoch
  started=$(docker inspect -f '{{.State.StartedAt}}' "$1")
  python3 -c 'import sys,datetime as d; s=sys.argv[1]; s=s[:26]+"Z" if len(s)>27 else s; t=d.datetime.fromisoformat(s.replace("Z","+00:00")).timestamp(); print(f"{float(sys.argv[2])-t:.2f}")' "$started" "$2"
}

echo "== 1. user namespace"
m=$(docker run --rm --label $L --runtime=sysbox-runc $ALPINE cat /proc/self/uid_map | awk '{print $1,$2,$3}')
r=$(docker run --rm --label $L --runtime=runc $ALPINE cat /proc/self/uid_map | awk '{print $1,$2,$3}')
info "sysbox uid_map: $m; runc uid_map: $r"
[ "$m" != "0 0 4294967295" ] && [ "${m%% *}" = 0 ] && pass "sysbox maps root to an unprivileged uid ($m)" || fail "identity uid_map: $m"

echo "== 2. docker:dind without restrictions"
docker run -d --label $L --name $P-dind --runtime=sysbox-runc -e DOCKER_TLS_CERTDIR= $DIND >/dev/null
t=$(wait_dockerd $P-dind) && info "inner dockerd ready in $(ready_seconds $P-dind "$t") s (docker:dind entrypoint)" || fail "inner dockerd did not start"
docker exec $P-dind docker run --rm hello-world 2>&1 | grep -q 'Hello from Docker!' && pass "hello-world in the inner engine" || fail "hello-world in the inner engine"
docker exec $P-dind docker info --format 'inner: {{.ServerVersion}} driver={{.Driver}} cgroup={{.CgroupVersion}} runtime={{.DefaultRuntime}}'
docker rm -fv $P-dind >/dev/null

echo "== 3. networks: internal attempt network plus a pull-through mirror peer"
docker network create --label $L $P-up >/dev/null
docker network create --label $L --internal $P-exec >/dev/null
docker run -d --label $L --name $P-mirror --network $P-up -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
  -e OTEL_TRACES_EXPORTER=none $REGISTRY >/dev/null
docker network connect --alias mirror $P-exec $P-mirror

# createArgs of the Docker backend with sysbox-runc: PID 1 is root of the user namespace,
# /run is an extra tmpfs, limits fit an inner engine. Comments name the runc differences.
# shellcheck disable=SC2054 # commas belong to --tmpfs options
policy_args=(--label "$L" --runtime=sysbox-runc --network="$P-exec" --read-only
  --user=0:0                      # runc: 65532:65532; dockerd needs namespace root
  --cap-drop=ALL --security-opt=no-new-privileges=true --cgroupns=private --ipc=private
  --memory=1g --memory-swap=1g --cpus=2
  --pids-limit=1024 --ulimit=nofile=65536:65536
  --restart=no --no-healthcheck --log-driver=none --workdir=/workspace
  --tmpfs=/workspace:rw,exec,nosuid,nodev,size=1073741824,mode=1777
  --tmpfs=/tmp:rw,noexec,nosuid,nodev,size=268435456,mode=1777
  --tmpfs=/run:rw,nosuid,nodev,size=67108864,mode=0755   # new: socket and engine state
  --shm-size=64m --env=HOME=/workspace
  --env=DIND_REGISTRY_MIRRORS=http://mirror:5000 --env=DIND_INSECURE_REGISTRIES=mirror:5000)
dind_cmd=(--entrypoint /usr/local/bin/docker-init "$SMOKE_IMAGE" -- /usr/local/libexec/dind)

wait_ready() { # name -> seconds from StartedAt to /run/dockerd.ready
  for _ in $(seq 300); do
    t=$(docker exec "$1" cat /run/dockerd.ready 2>/dev/null) && { ready_seconds "$1" "$t"; return 0; }
    [ "$(docker inspect -f '{{.State.Running}}' "$1")" = true ] || return 1
    sleep 0.1
  done
  return 1
}

echo "== 4. attempt policy under sysbox (cap-drop=ALL, NNP, read-only, user 0 plus exec 65532)"
vm pgrep -a sysbox-mgr | grep -q -- --relaxed-read-only && pass "sysbox-mgr runs with --relaxed-read-only" \
  || fail "sysbox-mgr without --relaxed-read-only: cgroup and /var/lib/docker stay read-only under --read-only"
c=$P-policy
docker create --name $c "${policy_args[@]}" "${dind_cmd[@]}" >/dev/null && docker start $c >/dev/null
if s=$(wait_ready $c); then
  pass "inner dockerd starts on a read-only root with cap-drop=ALL and no-new-privileges in $s s, no systemd"
else
  fail "inner dockerd did not start under the policy"; docker exec $c tail -20 /run/dockerd.log
fi
docker inspect $c --format 'INFO  HostConfig: runtime={{.HostConfig.Runtime}} capdrop={{.HostConfig.CapDrop}} secopt={{.HostConfig.SecurityOpt}} ro={{.HostConfig.ReadonlyRootfs}} user={{.Config.User}} mounts={{len .Mounts}}'
pid1=$(docker exec $c sh -c 'grep -E "^(CapEff|CapBnd|NoNewPrivs)" /proc/1/status | tr "\n" " "')
info "PID 1 (docker-init): $pid1"
case "$pid1" in *"CapEff:	0000000000000000"*) info "cap-drop applied to PID 1" ;; *) info "Sysbox CE did not apply --cap-drop=ALL to PID 1: full capabilities inside the namespace" ;; esac
ex=$(docker exec --user 65532:65532 $c sh -c 'id -u; grep -E "^(CapInh|CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs)" /proc/self/status | tr "\n" " "')
info "exec --user 65532: $ex"
case "$ex" in *"CapEff:	0000000000000000"*"NoNewPrivs:	1"*) pass "agent exec process: CapEff=0, NoNewPrivs=1" ;; *) fail "agent exec process has capabilities or lacks NNP" ;; esac
docker exec $c touch /etc/x 2>/dev/null && fail "root can write the root filesystem" || pass "root filesystem read-only even for namespace root"
docker exec --user 65532:65532 $c sh -c 'cp /bin/busybox /tmp/b 2>/dev/null; /tmp/b true' 2>/dev/null && fail "/tmp is executable" || pass "/tmp noexec"
docker exec $c sh -c 'grep -E " (/var/lib/docker|/run|/sys/fs/cgroup|/) " /proc/self/mounts | cut -d" " -f2-4 | cut -c1-90' | sed 's/^/INFO  mount: /'
docker exec --user 65532:65532 $c docker version --format '{{.Server.Version}}' >/dev/null && pass "agent (65532) drives the inner engine through /run/docker.sock" || fail "agent cannot reach the inner docker.sock"

echo "== 5. no internet, pulls through the mirror peer"
docker exec $c sh -c 'wget -q -T 5 -O /dev/null https://registry-1.docker.io/v2/' 2>/dev/null && fail "the attempt reaches the internet" || pass "no internet from the attempt (internal network, no external names)"
docker exec --user 65532:65532 $c docker pull -q hello-world >/dev/null 2>&1 && pass "inner pull of hello-world through registry-mirrors (peer mirror:5000)" || fail "inner pull through the mirror"
docker exec --user 65532:65532 $c docker pull -q ghcr.io/linuxcontainers/alpine:latest >/dev/null 2>&1 && info "ghcr reachable (unexpected)" || pass "ghcr unreachable without its own proxy (registry-mirrors cover Docker Hub only)"
docker exec --user 65532:65532 $c docker run --rm hello-world 2>&1 | grep -q 'Hello from Docker!' && pass "inner container runs (writable cgroup, parent NNP)" || fail "inner run"
docker exec --user 65532:65532 $c sh -c 'docker run -d --name pg -e POSTGRES_PASSWORD=x postgres:17-alpine >/dev/null 2>&1 && for i in $(seq 60); do docker exec pg pg_isready -q 2>/dev/null && exit 0; sleep 1; done; exit 1' \
  && pass "postgres in the inner engine (su-exec under parent NNP)" || fail "inner postgres"
docker exec --user 65532:65532 $c sh -c 'docker run --rm -p 5433:5432 -d --name pg2 -e POSTGRES_PASSWORD=x postgres:17-alpine >/dev/null 2>&1; sleep 1; docker port pg2' | sed 's/^/INFO  inner port publish: /'
docker exec --user 65532:65532 $c sh -c 'docker run --rm --privileged alpine:3.22 sh -c "cat /proc/self/uid_map; grep CapEff /proc/self/status"' 2>&1 | tail -2 | sed 's/^/INFO  inner --privileged: /'

echo "== 6. where /var/lib/docker lives, and cleanup"
id=$(docker inspect -f '{{.Id}}' $c)
vm sh -c "grep -E ' /var/lib/docker ' /proc/\$(docker inspect -f '{{.State.Pid}}' $id)/mountinfo | cut -d' ' -f4,5; du -sh /var/lib/sysbox/docker/$id" | sed 's/^/INFO  vm: /'
cg=$(vm sh -c "cat /sys/fs/cgroup/system.slice/docker-$id.scope/memory.peak /sys/fs/cgroup/system.slice/docker-$id.scope/memory.current" | tr '\n' ' ')
info "attempt memory.peak/current with inner postgres (bytes): $cg"
docker rm -fv $c >/dev/null
# sysbox-mgr removes the data asynchronously, after `docker rm` returns.
left=$(vm sh -c "for i in \$(seq 60); do l=\$(find /var/lib/sysbox -maxdepth 2 -name '$id*'); [ -z \"\$l\" ] && { echo 0 \$i; exit; }; sleep 0.5; done; echo \$l")
case "$left" in "0 "*) pass "inner engine data removed from /var/lib/sysbox after docker rm -fv (in ~$(( ${left#0 } / 2 )) s)" ;; *) fail "data left: $left" ;; esac

echo "== 7. SYSBOX_HONOR_CAPS=TRUE with cap-drop=ALL"
c=$P-honor
docker create --name $c "${policy_args[@]}" --log-driver=local -e SYSBOX_HONOR_CAPS=TRUE "${dind_cmd[@]}" >/dev/null && docker start $c >/dev/null
if s=$(wait_ready $c); then info "dockerd starts with honoured cap-drop too ($s s)"; else info "dockerd does not work with honoured cap-drop=ALL: $(docker logs $c 2>&1 | grep -v '^$' | tail -1 | cut -c1-200)"; fi
docker rm -fv $c >/dev/null

echo "== result: $([ $rc = 0 ] && echo OK || echo FAILED)"
exit $rc
