#!/bin/bash
# Provision a Sysbox host (ADR 0019): pinned Docker CE with the containerd image store,
# cgroup v2, Sysbox CE 0.7.1 checked against its release sha256, and sysbox-mgr with
# --relaxed-read-only. Idempotent. Lima runs it as root on first boot; to rerun:
#   limactl shell sysbox sudo bash /tmp/provision.sh
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

DOCKER_VERSION=5:29.9.0-1~ubuntu.24.04~noble
CONTAINERD_VERSION=2.4.1-2~ubuntu.24.04~noble
BUILDX_VERSION=0.38.0-1~ubuntu.24.04~noble
COMPOSE_VERSION=5.6.0-1~ubuntu.24.04~noble
DOCKER_GPG_FINGERPRINT=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
SYSBOX_VERSION=0.7.1
SYSBOX_SHA256_ARM64=04ca894ae0b53f0fa54eaacc173ce40363c9a95ea5450f773716a84ef650a69b
SYSBOX_SHA256_AMD64=9d6d5484f980d0a17f86c492c1262015c2afb66280bdb97215b79fde6a0261c5
MARKER=/var/lib/sysbox-host/provisioned

[ -e "$MARKER" ] && { echo "already provisioned"; exit 0; }

test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs || { echo "cgroup v2 required" >&2; exit 1; }
arch=$(dpkg --print-architecture)
case "$arch" in
  arm64) sysbox_sha=$SYSBOX_SHA256_ARM64 ;;
  amd64) sysbox_sha=$SYSBOX_SHA256_AMD64 ;;
  *) echo "unsupported arch $arch" >&2; exit 1 ;;
esac
# Lima renders provision scripts as Go templates and substitutes the user below; this
# script therefore contains no other double braces. A manual sudo run uses SUDO_USER.
lima_user="{{.User}}"
# shellcheck disable=SC1083 # a literal brace: the template was not rendered
case $lima_user in *.User}*) lima_user=${SUDO_USER:-} ;; esac

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg jq rsync fuse3

# Docker CE from the official repository; the key is checked by fingerprint.
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /tmp/docker.asc
got=$(gpg --show-keys --with-colons /tmp/docker.asc | awk -F: '/^fpr:/{print $10; exit}')
[ "$got" = "$DOCKER_GPG_FINGERPRINT" ] || { echo "docker gpg fingerprint mismatch: $got" >&2; exit 1; }
gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg /tmp/docker.asc
echo "deb [arch=$arch signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu noble stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update

# daemon.json before the first dockerd start: containerd image store, bounded logs.
install -d -m 0755 /etc/docker
[ -s /etc/docker/daemon.json ] || cat > /etc/docker/daemon.json <<'JSON'
{
  "features": {"containerd-snapshotter": true},
  "log-driver": "local",
  "log-opts": {"max-size": "10m", "max-file": "3"}
}
JSON
apt-get install -y --no-install-recommends \
  "docker-ce=$DOCKER_VERSION" "docker-ce-cli=$DOCKER_VERSION" "containerd.io=$CONTAINERD_VERSION" \
  "docker-buildx-plugin=$BUILDX_VERSION" "docker-compose-plugin=$COMPOSE_VERSION"
apt-mark hold docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

# The socket forwarded to the host belongs to the Lima user, as in Lima's docker template.
if [ -n "$lima_user" ]; then
  install -d /etc/systemd/system/docker.socket.d
  printf '[Socket]\nSocketUser=%s\n' "$lima_user" > /etc/systemd/system/docker.socket.d/override.conf
  usermod -aG docker "$lima_user"
fi
systemctl daemon-reload
systemctl enable --now docker.socket docker.service
systemctl restart docker.socket docker.service

# The Sysbox post-install script restarts dockerd and refuses hosts with containers.
if ! dpkg-query -W -f='${db:Status-Status}' sysbox-ce 2>/dev/null | grep -qx installed; then
  [ -z "$(docker ps -aq)" ] || { echo "sysbox post-install needs a host without containers" >&2; exit 1; }
  deb=/tmp/sysbox-ce_${SYSBOX_VERSION}.linux_${arch}.deb
  curl -fsSL -o "$deb" "https://github.com/nestybox/sysbox/releases/download/v${SYSBOX_VERSION}/sysbox-ce_${SYSBOX_VERSION}.linux_${arch}.deb"
  echo "$sysbox_sha  $deb" | sha256sum -c -
  apt-get install -y "$deb"
  rm -f "$deb"
fi

# Attempts run with --read-only. Without this flag Sysbox also mounts /sys/fs/cgroup and
# the implicit /var/lib/docker read-only, and the inner engine cannot start containers.
# Restart Sysbox only while no Sysbox containers exist.
dropin=/etc/systemd/system/sysbox-mgr.service.d/relaxed-read-only.conf
if [ ! -e "$dropin" ]; then
  install -d "${dropin%/*}"
  printf '[Service]\nExecStart=\nExecStart=/usr/bin/sysbox-mgr --relaxed-read-only\n' > "$dropin"
  systemctl daemon-reload
  systemctl restart sysbox
fi
pgrep -a sysbox-mgr | grep -q -- --relaxed-read-only

docker info --format json | jq -e '.Runtimes | has("sysbox-runc")' >/dev/null
docker info --format json | jq -r '"\(.ServerVersion) cgroup=\(.CgroupVersion) driver=\(.Driver)"'
systemctl is-active sysbox-mgr sysbox-fs
install -d -m 0755 "${MARKER%/*}"
date -u +%FT%TZ > "$MARKER"
