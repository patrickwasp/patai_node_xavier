#!/usr/bin/env bash
# Tests for workloads/chat-postgres/patai-postgres with real Postgres images, on a PC's Docker, not a node: a cluster
# for each major and one-way change, taken over into a new folder with the old one kept, a rollback finding its own, an
# update again after one, letting go once a later release runs, refusing a newer cluster, and a restore's dump.
#
#   tests/patai-postgres.sh
set -euo pipefail
recipe=$(cd "$(dirname "$0")/../workloads/chat-postgres" && pwd)
work=$(mktemp -d)
data=$work/chat-postgres
name=patai-postgres-test
trap 'docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
mkdir -p "$data"

pg17=pgvector/pgvector:0.8.7-pg17-trixie@sha256:89947206f52eac3a9df9b663df7e76987cbdc623be6576b3a524953d568700d7
# Postgres 17 with this entrypoint, as a release before a new major would have had it.
sed "s|^FROM .*|FROM $pg17|; s|^ARG CLUSTER=18|ARG CLUSTER=17|" "$recipe/Dockerfile" \
  | docker build -q -t "$name:17" -f - "$recipe" >/dev/null
docker build -q -t "$name:18" --build-arg UPGRADES=17 "$recipe" >/dev/null
docker build -q -t "$name:18.1" --build-arg CLUSTER=18.1 "$recipe" >/dev/null

fail() { echo "FAIL: $*" >&2; docker logs "$name" 2>&1 | tail -20 >&2 || true; exit 1; }
start() { # <tag> <release> [folder]
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --user "$(id -u):$(id -g)" -e POSTGRES_USER=patai -e POSTGRES_DB=patai_chat \
    -e POSTGRES_PASSWORD=test -e PATAI_RELEASE="$2" -v "${3:-$data}:/var/lib/postgresql" "$name:$1" >/dev/null
  for _ in $(seq 120); do
    # Over TCP: the server the entrypoint starts to make the database answers on its socket alone.
    docker exec "$name" pg_isready -q -h 127.0.0.1 -U patai -d patai_chat 2>/dev/null && return 0
    [[ $(docker inspect -f '{{.State.Running}}' "$name") == true ]] || return 1
    sleep 1
  done
  return 1
}
stop() { docker stop -t 30 "$name" >/dev/null; }
sql() { docker exec "$name" psql -X -At -U patai -d patai_chat -c "$1"; }
current() { cat "$data/current"; }
has() { [[ -s $data/$1/docker/PG_VERSION ]]; }

echo "Postgres 17, a new database"
start 17 a || fail "17 didn't start"
sql "create extension vector; create table t (id int, v vector(3)); insert into t values (1, '[1,2,3]')" >/dev/null
stop
has 17 && [[ $(current) == 17 ]] || fail "no 17 cluster"

echo "An update to 18: pg_upgrade into its own folder, 17 left as it was"
start 18 b || fail "18 didn't take over 17"
[[ $(sql 'select id from t order by v <-> $$[1,2,3]$$ limit 1') == 1 ]] || fail "the data didn't come over"
[[ $(sql 'select current_setting($$server_version_num$$)::int / 10000') == 18 ]] || fail "not 18"
docker exec "$name" psql -X -At "postgresql://patai:test@127.0.0.1/patai_chat" -c 'select 1' >/dev/null \
  || fail "who may connect didn't come over"
grep -qx from=17 "$data/18/moved" && grep -qx release=b "$data/18/moved" || fail "no record of the move"
sql "insert into t values (2, '[2,2,2]')" >/dev/null
stop
has 17 && has 18 && [[ $(current) == 18 ]] || fail "17 or 18 missing after the update"

echo "A rollback to 17 finds its own"
start 17 a || fail "17 didn't start after the rollback"
[[ $(sql 'select count(*) from t') == 1 ]] || fail "17 isn't as it was"
sql "insert into t values (3, '[3,3,3]')" >/dev/null
stop
[[ $(current) == 17 ]] || fail "current isn't 17"

echo "Updating again takes over what 17 has since, and sets the old 18 aside"
start 18 b || fail "18 didn't start again"
[[ $(sql 'select string_agg(id::text, $$,$$ order by id) from t') == 1,3 ]] || fail "18 isn't 17's data now"
ls -d "$data"/18.stale-* >/dev/null || fail "the old 18 wasn't set aside"
stop

echo "A one-way change within 18: copied into 18.1"
start 18.1 c || fail "18.1 didn't take over 18"
[[ $(sql 'select string_agg(id::text, $$,$$ order by id) from t') == 1,3 ]] || fail "18.1 isn't 18's data"
grep -qx from=18 "$data/18.1/moved" || fail "no record of the copy"
stop
has 17 && has 18 && has 18.1 || fail "an older cluster went while the release that moved it runs"

echo "A later release lets go of the older ones"
start 18.1 d || fail "18.1 didn't start for a later release"
sql 'select 1' >/dev/null
stop
! has 17 && ! has 18 && ! ls -d "$data"/18.stale-* >/dev/null 2>&1 && has 18.1 || fail "the older ones are still there"

echo "Back to 17, whose folder is gone: refused, nothing made"
if start 17 e; then fail "17 started on a newer cluster"; fi
docker logs "$name" 2>&1 | grep -q "newer than this image's 17" || fail "no reason given"
! has 17 || fail "an empty 17 was made"
[[ $(current) == 18.1 ]] || fail "current moved"

echo "A restore's dump, into a folder of its own"
start 18.1 d || fail "18.1 didn't start"
mkdir -p "$work/restore"
docker exec "$name" pg_dump --format=custom -U patai -d patai_chat > "$work/restore/restore.dump"
stop
start 18 f "$work/restore" || fail "the restore didn't load"
[[ $(sql 'select string_agg(id::text, $$,$$ order by id) from t') == 1,3 ]] || fail "the dump isn't what was saved"
[[ ! -e $work/restore/restore.dump && $(cat "$work/restore/current") == 18 ]] || fail "the restore left its dump"
stop
echo "OK"
