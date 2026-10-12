#!/bin/sh
# Install the attempt memory pool on a Linux host (ADR 0020), as root:
#   pool.sh SLICE_FILE [SWAP_SIZE]    e.g. pool.sh multica_attempts.slice 4G
# Adds a swap file when the host has no swap, installs and starts the slice and checks
# that dockerd and containerd keep their package OOM protection. Idempotent.
set -eu
slice=${1:?usage: pool.sh SLICE_FILE [SWAP_SIZE]}
name=$(basename "$slice")
if [ -z "$(swapon --noheadings --show=NAME)" ]; then
  fallocate -l "${2:-4G}" /swapfile
  chmod 600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi
install -m 0644 "$slice" "/etc/systemd/system/$name"
systemctl daemon-reload
systemctl enable --now "$name"
dir=/sys/fs/cgroup/$name
for f in memory.high memory.max memory.swap.max; do printf '%s=%s ' "$f" "$(cat "$dir/$f")"; done
echo
# A pool OOM must never reach the engine: attempts run with oom_score_adj 0.
for unit in docker containerd; do
  adj=$(systemctl show "$unit" -p OOMScoreAdjust --value)
  [ "${adj:-0}" -lt 0 ] || { echo "$unit has no OOMScoreAdjust; add a negative one" >&2; exit 1; }
done
swapon --show
