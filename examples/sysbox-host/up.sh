#!/bin/sh
# Start the Lima VM `sysbox` (Docker CE + Sysbox CE) and create the docker context
# `lima-sysbox`. The current context is not switched.
#   examples/sysbox-host/up.sh          create or start the VM, check Sysbox, add the context
#   examples/sysbox-host/up.sh stop     stop the VM (the disk stays)
#   examples/sysbox-host/up.sh delete   delete the VM and the context
#   examples/sysbox-host/up.sh pool     add swap and the attempt pool slice (pool.sh)
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
name=${LIMA_SYSBOX_NAME:-sysbox}
ctx=lima-$name

case "${1:-up}" in
  stop) exec limactl stop "$name" ;;
  delete) limactl delete --force "$name"; docker context rm -f "$ctx" 2>/dev/null || true; exit 0 ;;
  pool)
    limactl copy "$here/pool.sh" "$here/multica_attempts.slice" "$name:/tmp/"
    exec limactl shell "$name" sudo sh /tmp/pool.sh /tmp/multica_attempts.slice 4G ;;
  up) ;;
  *) echo "usage: $0 [up|stop|delete|pool]" >&2; exit 2 ;;
esac

command -v limactl >/dev/null || { echo "Lima is required: https://lima-vm.io" >&2; exit 1; }
if limactl list -q 2>/dev/null | grep -qx "$name"; then
  limactl start "$name"
else
  limactl start --tty=false --name="$name" "$here/lima.yaml"
fi

sock=$(limactl list "$name" --format '{{.Dir}}/sock/docker.sock')
docker context inspect "$ctx" >/dev/null 2>&1 || docker context create "$ctx" --description "Lima $name: Docker + Sysbox" --docker "host=unix://$sock" >/dev/null
docker --context "$ctx" info --format 'engine={{.ServerVersion}} cgroup={{.CgroupVersion}} runtimes={{range $k, $v := .Runtimes}}{{$k}} {{end}}'
docker --context "$ctx" info --format '{{json .Runtimes}}' | grep -q sysbox-runc || { echo "sysbox-runc is not registered" >&2; exit 1; }
echo "ready: docker --context $ctx ..."
