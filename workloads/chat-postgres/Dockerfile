# Postgres with pgvector, as pgvector ships it, for Chat's people, chats and documents (docs/apps.md, The Chat app).
FROM pgvector/pgvector:0.8.7-pg18-trixie@sha256:ab49524d18df7200b74ac9a475e1db6297fc1eb57da4c214c63d10ecc4b754fa
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
# Each cluster in a folder of its own, as its manifest's data.cluster says (docs/updates.md, Data across slots): this
# major's, or after a dot a one-way change within it, such as pgvector's ALTER EXTENSION UPDATE. UPGRADES are the older
# majors it takes a cluster over from with pg_upgrade, its manifest's data.upgrades: their server and pgvector, from the
# PostgreSQL project's apt repository, which the image has.
ARG CLUSTER=18
ARG UPGRADES=
RUN if [ -n "$UPGRADES" ]; then \
      sed -i 's/ main 18$/ main/' /etc/apt/sources.list.d/pgdg.list \
      && apt-get update \
      && for major in $UPGRADES; do \
           apt-get install -y --no-install-recommends postgresql-$major postgresql-$major-pgvector || exit 1; done \
      && rm -rf /var/lib/apt/lists/*; \
    fi
ENV PATAI_CLUSTER=$CLUSTER PATAI_UPGRADES=$UPGRADES PGDATA=/var/lib/postgresql/$CLUSTER/docker
# A restore's dump, loaded into the empty database at the first start (docs/console.md, Backups). The node builds with
# Docker's legacy builder, which keeps the file's mode: it's executable in the repo.
COPY restore.sh /docker-entrypoint-initdb.d/zz-patai-restore.sh
COPY patai-postgres /usr/local/bin/patai-postgres
ENTRYPOINT ["patai-postgres"]
CMD ["postgres"]
