#!/usr/bin/env bash
# Tests for bundle/hook's install-check on a PC's Docker, not a node: a "unit" in focal, as a node is, with the running
# release's recipes in /usr/share/patai, its apps' data on /data, and a bundle beside the hook holding its own
# patai-formats and data-formats.json, as scripts/build-bundle makes it (docs/updates.md, Data across slots).
#
#   tests/bundle-hook.sh
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
go_image=$(sed -n 's/^FROM \(golang:[^ ]*\).*/\1/p' "$repo/docker/console.Dockerfile")
docker run --rm -v "$repo/console:/src:ro" -w /src -v patai-go-mod:/go/pkg/mod -v patai-go-build:/root/.cache/go-build \
  -v "$work:/out" -e CGO_ENABLED=0 "$go_image" go build -o /out/patai-formats ./cmd/patai-formats

failures=0
# <name> <want: 0 or 10> <running chat version/floor> <bundle's> [record version/floor] [clusters: here current
# running bundle upgrades]
check() {
  local name=$1 want=$2 running=$3 next=$4 record=${5:-} pg=${6:-}
  local unit=$work/unit bundle=$work/bundle
  rm -rf "$unit" "$bundle"
  mkdir -p "$unit/share/workloads" "$unit/data" "$unit/persist" "$bundle"
  cp "$repo/console/STATE_FORMAT" "$unit/share/state-format"
  cp "$repo/console/STATE_FORMAT" "$bundle/state-format"
  cp "$repo/bundle/hook" "$work/patai-formats" "$bundle/"
  cp -r "$repo/workloads/chat-server" "$repo/workloads/chat-postgres" "$unit/share/workloads/"
  local here="" current="" was=18 will=18 upgrades=""
  [[ -n $pg ]] && read -r here current was will upgrades <<< "$pg"
  python3 - "$unit/share/workloads" "$running" "$was" <<'PY'
import json, sys
folder, running, cluster = sys.argv[1:]
version, floor = map(int, running.split("/"))
for name, change in (("chat-server", {"format": {"version": version, "floor": floor}}), ("chat-postgres", {"cluster": cluster})):
    path = f"{folder}/{name}/manifest.json"
    m = json.load(open(path))
    m["data"].update(change)
    json.dump(m, open(path, "w"))
PY
  local version=${next%/*} floor=${next#*/}
  printf '{"chat-server": {"title": "Chat server", "format": {"version": %s, "floor": %s}}, "chat-postgres": {"title": "Postgres", "cluster": "%s"%s}}\n' \
    "$version" "$floor" "$will" "${upgrades:+, \"upgrades\": [$upgrades]}" > "$bundle/data-formats.json"
  if [[ -n $record ]]; then
    mkdir -p "$unit/data/chat-server"
    printf '{"version": %s, "floor": %s, "state": "ready"}\n' "${record%/*}" "${record#*/}" > "$unit/data/chat-server/format.json"
  fi
  for c in ${here//,/ }; do
    mkdir -p "$unit/data/chat-postgres/$c/docker" && echo "${c%%.*}" > "$unit/data/chat-postgres/$c/docker/PG_VERSION"
  done
  [[ -z $current ]] || echo "$current" > "$unit/data/chat-postgres/current"
  local got=0 said
  said=$(docker run --rm -v "$unit/share:/usr/share/patai:ro" -v "$unit/data:/data:ro" -v "$unit/persist:/persist:ro" \
    -v "$bundle:/bundle:ro" -e RAUC_MF_COMPATIBLE=patai-node-xavier -e RAUC_SYSTEM_COMPATIBLE=patai-node-xavier \
    ubuntu:20.04@sha256:8feb4d8ca5354def3d8fce243717141ce31e2c428701f6682bd2fafe15388214 /bundle/hook install-check 2>&1) || got=$?
  if [[ $got == "$want" ]]; then
    echo "ok   $name${said:+: $said}"
  else
    echo "FAIL $name: exit $got, want $want: $said"
    failures=$((failures + 1))
  fi
}

check "an update that only adds" 0 3/2 4/3 3/2 "18 18 18 18"
check "a release the running one couldn't roll back from" 10 3/2 4/4 3/2
check "the same with no data yet" 10 3/2 4/4
check "a downgrade one step back" 0 4/3 3/2 4/3
check "a downgrade below the data's floor" 10 5/4 3/2 5/4
check "a new Postgres major, its folder kept" 0 3/2 3/2 3/2 "18 18 18 19 18"
check "a new Postgres major that can't take over" 10 3/2 3/2 3/2 "18 18 18 19"
check "back to a Postgres whose folder is gone" 10 3/2 3/2 3/2 "19 19 19 18"
check "back to a Postgres whose folder is kept" 0 3/2 3/2 3/2 "18,19 19 19 18"
(( failures == 0 )) || { echo "$failures failed" >&2; exit 1; }
echo OK
