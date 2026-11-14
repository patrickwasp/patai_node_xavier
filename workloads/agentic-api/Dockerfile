# vLLM's agentic-api for the Xavier (docs/apps.md, The agentic API): the gateway alone, from a release's source.
# Built on the unit, as there's no published image or wheel for arm64 Linux. Its code interpreter is left out: it needs
# cgroup v2, which this kernel doesn't give containers.
FROM rust:1.98.1-slim-bookworm@sha256:ff521445a372125ed4f76e1453a1f8098f2d05332d1601d30db1c1f62757e730 AS build

ARG AGENTIC_VERSION=v0.10.0

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates pkg-config build-essential libssl-dev \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 -b "$AGENTIC_VERSION" https://github.com/vllm-project/agentic-api /src
WORKDIR /src
RUN cargo build --locked --release -p agentic-server \
 && install -Dm755 -s target/release/agentic-server /out/agentic-server

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates libssl3 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/agentic-server /usr/local/bin/agentic-server
# Its conversations, in /data/agentic-api on the unit, which belongs to this user.
USER 10001:10001
ENV AGENTIC_API_HOME=/var/lib/agentic-api
WORKDIR /var/lib/agentic-api
ENTRYPOINT ["/usr/local/bin/agentic-server"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
