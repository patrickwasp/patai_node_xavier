#!/usr/bin/env bash
# The image's entrypoint runs this once, when it has made an empty cluster and the recipe's database, with a server on
# its socket alone: nothing else reaches it until this is done. A restore leaves the database's dump beside the cluster
# (patai_node_xavier docs/console.md, Backups), which this loads whole or not at all.
set -Eeuo pipefail
dump=/var/lib/postgresql/restore.dump
[[ -f $dump ]] || exit 0
echo "patai: loading the backup's database"
if ! pg_restore --exit-on-error --single-transaction --no-owner --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" "$dump"; then
  # With no cluster, the next start makes one and tries again, rather than starting with an empty database.
  echo "patai: the backup's database didn't load; it's tried again at the next start" >&2
  rm -rf "${PGDATA:?}"
  exit 1
fi
rm -f "$dump"
echo "patai: the backup's database is loaded"
