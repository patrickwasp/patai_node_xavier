#!/usr/bin/env bash
# Boot health check, run by NVIDIA's l4t-rootfs-validation-config.service. A non-zero exit reboots;
# after 3 tries UEFI rolls back to the other slot. Check only what this rootfs must provide itself:
# not the network, because a pulled cable must not roll a unit back, and not /data, because the
# core OS doesn't depend on the NVMe.
#
# Wait for sshd rather than trust the unit's ordering: on a slot's first boot NVIDIA's nvfb.sh
# restarts it (dpkg-reconfigure openssh-server), and systemd doesn't hold an After= unit for a
# restart. Without this every fresh image fails here.
#
# The console is how an owner manages a unit, so a release with a dead console is a bad release.
# /healthz proves the UI, the state store and the root helper. It passes in recovery mode only for an
# identified storage fault, which would be the same on both slots (docs/console.md, Boot validation).
# Changes from people wait for /run/patai/validated.
set -euo pipefail
for _ in {1..60}; do
  systemctl is-active --quiet ssh && break
  sleep 1
done
systemctl is-active --quiet ssh
for _ in {1..60}; do
  curl -fsSk --max-time 5 https://127.0.0.1/healthz >/dev/null && break
  sleep 1
done
curl -fsSk --max-time 10 https://127.0.0.1/healthz >/dev/null
rauc status mark-good
install -d /run/patai
touch /run/patai/validated
