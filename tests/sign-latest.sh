#!/usr/bin/env bash
# scripts/build-bundle and scripts/sign-latest end to end, on a PC, with a throwaway fleet key and tiny images that hold
# only what a bundle takes from one: the bundle carries the image's patai-formats and data-formats.json beside its
# hook, latest.json signs what it declares, and a release the channel's units couldn't use is refused (docs/updates.md,
# Data across slots).
#
#   tests/sign-latest.sh
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
source "$repo/scripts/lib.sh"
work=$(mktemp -d)
keys=$(mktemp -d -p /dev/shm patai-test-keys.XXXXXX)
trap 'docker run --rm -v "$work:/work" "$(rauc_image amd64)" rm -rf /work/out; rm -rf "$work" "$keys"' EXIT
export PATAI_OUT=$work/out PATAI_RAUC_KEYS=$keys

(cd "$keys" && openssl req -x509 -newkey rsa:4096 -nodes -sha256 -days 2 -subj "/CN=test CA" \
    -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign \
    -keyout ca-key.pem -out ca.pem 2>/dev/null \
  && openssl req -newkey rsa:4096 -nodes -sha256 -subj "/CN=test signing" -keyout key.pem -out signing.csr 2>/dev/null \
  && printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=codeSigning\n' > ext \
  && openssl x509 -req -sha256 -days 2 -in signing.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -extfile ext \
    -out cert.pem 2>/dev/null)

# A release whose image declares Chat's data at this version and floor.
release() {
  local version=$1 format=$2 out=$PATAI_OUT/$1 tree=$work/tree-$1
  mkdir -p "$out" "$tree/usr/local/sbin" "$tree/usr/share/patai"
  echo '#!/bin/sh' > "$tree/usr/local/sbin/patai-formats"
  chmod 755 "$tree/usr/local/sbin/patai-formats"
  printf '{"chat-server": {"title": "Chat server", "format": %s}, "chat-postgres": {"title": "Postgres", "cluster": "18"}}\n' \
    "$format" > "$tree/usr/share/patai/data-formats.json"
  # Enough that its squashfs is more than a block.
  head -c 262144 /dev/urandom > "$tree/filler"
  truncate -s 8M "$out/rootfs.ext4"
  mkfs.ext4 -q -d "$tree" "$out/rootfs.ext4"
  cp "$keys/ca.pem" "$out/keyring.pem"
  echo $(( $(date -u +%s) - 60 )) > "$out/built-at"
  "$repo/scripts/build-bundle" "$1" >/dev/null
}

channel=$work/channel
mkdir -p "$channel"
fail() { echo "FAIL: $*" >&2; exit 1; }

release 2026.12.01-1 '{"version": 3, "floor": 2}'
docker run --rm -v "$PATAI_OUT:/release:ro" "$(rauc_image amd64)" unsquashfs -l /release/2026.12.01-1/patai-2026.12.01-1.raucb \
  | grep -q 'squashfs-root/patai-formats' || fail "the bundle has no patai-formats"
"$repo/scripts/sign-latest" stable 2026.12.01-1 "$channel" >/dev/null || fail "the channel's first wasn't signed"
python3 -I -c 'import base64, json, sys
p = json.loads(base64.b64decode(json.load(open(sys.argv[1]))["payload"]))
assert p["data"]["chat-server"]["format"] == {"version": 3, "floor": 2}, p
assert p["data"]["chat-postgres"]["cluster"] == "18", p' "$channel/latest.json" || fail "latest.json doesn't sign the data"
echo "ok   a release's data is signed into latest.json"

sleep 1
release 2026.12.02-1 '{"version": 5, "floor": 5}'
if "$repo/scripts/sign-latest" stable 2026.12.02-1 "$channel" 2> "$work/said"; then fail "a release 3 couldn't roll back from was offered"; fi
grep -q "couldn't roll back" "$work/said" || fail "no reason: $(cat "$work/said")"
echo "ok   refused: $(grep -m1 "Chat server:" "$work/said")"

sleep 1
release 2026.12.03-1 '{"version": 4, "floor": 3}'
"$repo/scripts/sign-latest" stable 2026.12.03-1 "$channel" >/dev/null || fail "the next one wasn't signed"
echo "ok   the next release, which 3 can roll back from"

sleep 1
release 2026.11.30-1 '{"version": 2, "floor": 1}'
if "$repo/scripts/sign-latest" stable 2026.11.30-1 "$channel" 2> "$work/said"; then fail "a release older than 4's floor was offered"; fi
grep -q "too old" "$work/said" || fail "no reason: $(cat "$work/said")"
echo "ok   refused: $(grep -m1 "Chat server:" "$work/said")"
echo OK
