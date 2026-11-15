# Open Terminal's slim image, for the Chat app's terminal (docs/apps.md, The Chat app): a shell, files and code for one
# person, in a container of their own. Upstream's Dockerfile.slim, built from its release's source, checked by its hash,
# without what its egress filter needs: the node's firewall does that job, and the filter needs NET_ADMIN.
FROM python:3.12.16-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b AS builder
ARG VERSION=0.17.13
ARG SOURCE_SHA256=4b615e4b3b8308e99d3d558f1e04bb8ee38f21c1b6572293ca656d02e086792a
RUN apt-get update && apt-get install -y --no-install-recommends build-essential gcc libffi-dev curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /source.tar.gz "https://github.com/open-webui/open-terminal/archive/refs/tags/v${VERSION}.tar.gz" \
    && echo "${SOURCE_SHA256}  /source.tar.gz" | sha256sum -c - \
    && mkdir /src && tar -xzf /source.tar.gz -C /src --strip-components=1
WORKDIR /src
RUN pip install --no-cache-dir --prefix=/install .

FROM python:3.12.16-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
RUN apt-get update && apt-get install -y --no-install-recommends tini gosu curl git jq less procps ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /install /usr/local
COPY --from=builder /src /app
WORKDIR /app
RUN pip install --no-cache-dir --no-deps . \
    && pip uninstall -y pip setuptools 2>/dev/null || true \
    && find /usr/local/lib/python3.12 -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
RUN useradd -m -s /bin/bash -u 1000 user && chmod +x /app/entrypoint-slim.sh
ENV SHELL=/bin/bash
EXPOSE 8000
ENTRYPOINT ["/usr/bin/tini", "--", "/app/entrypoint-slim.sh"]
CMD ["run"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
