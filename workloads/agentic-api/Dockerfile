# vLLM's agentic-api for the Xavier (docs/apps.md, The agentic API): the gateway alone, from a release's source, with
# its code interpreter, which runs Python in Eryx: CPython compiled to WebAssembly, in Wasmtime. There's no published
# image or wheel for arm64 Linux.
#
# Built on a PC for arm64, the Rust cross-compiled, and loaded on a node to publish:
#   docker buildx build --platform linux/arm64 --load -t <ref> workloads/agentic-api
#   docker save <ref> | scripts/ssh docker load
#   scripts/publish-workloads agentic-api
# It builds on a node too, where nothing is cross-compiled, but takes the node's memory and cores for half an hour.
#
# patai.patch, on top of the release: Wasmtime 48.0.9 for its Cranelift fixes, one of them for aarch64
# (GHSA-mghf-wjfp-p5xm); a worker killed without cgroup.kill, which Linux has from 5.14 and the Xavier's 5.10 lacks;
# one core per run; and patai-eryx-precompile, which compiles Eryx's runtime with the same Eryx and Wasmtime.
FROM --platform=$BUILDPLATFORM rust:1.98.1-slim-bookworm@sha256:ff521445a372125ed4f76e1453a1f8098f2d05332d1601d30db1c1f62757e730 AS build

ARG BUILDARCH
ARG AGENTIC_VERSION=v0.10.0
ARG AGENTIC_COMMIT=b0f491e024ff73339b16e6f83a301041a51257d7
ARG ERYX_VERSION=0.8.0
ARG ERYX_RUNTIME_SHA256=28da1719b4b0e5af05380ecd400ea3944cb26b7c918e363a32d4fad38422abe5

RUN if [ "$BUILDARCH" != arm64 ]; then dpkg --add-architecture arm64; fi \
 && apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl pkg-config build-essential libssl-dev:arm64 \
    $([ "$BUILDARCH" != arm64 ] && echo gcc-aarch64-linux-gnu libc6-dev-arm64-cross) \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 -b "$AGENTIC_VERSION" https://github.com/vllm-project/agentic-api /src \
 && test "$(git -C /src rev-parse HEAD)" = "$AGENTIC_COMMIT"
WORKDIR /src
COPY patai.patch /patai.patch
RUN git apply /patai.patch && rustup target add aarch64-unknown-linux-gnu
ENV CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER=aarch64-linux-gnu-gcc \
    CC_aarch64_unknown_linux_gnu=aarch64-linux-gnu-gcc \
    AR_aarch64_unknown_linux_gnu=aarch64-linux-gnu-ar \
    PKG_CONFIG_aarch64_unknown_linux_gnu=pkg-config \
    PKG_CONFIG_PATH_aarch64_unknown_linux_gnu=/usr/lib/aarch64-linux-gnu/pkgconfig \
    PKG_CONFIG_ALLOW_CROSS=1

# Eryx's runtime, checked against its release, compiled for the Xavier with Cranelift's baseline settings for
# aarch64: a node's Carmel cores have every feature that assumes, and nothing is compiled for the build machine's.
RUN curl -fsSL -o /runtime.wasm \
      "https://github.com/eryx-org/eryx/releases/download/eryx-v$ERYX_VERSION/runtime.wasm" \
 && echo "$ERYX_RUNTIME_SHA256  /runtime.wasm" | sha256sum -c - \
 && ERYX_PRECOMPILE_BOOTSTRAP=1 cargo run --locked --release -p patai-eryx-precompile -- \
      /runtime.wasm /runtime.cwasm aarch64-unknown-linux-gnu \
 && rm -rf target/release

RUN ERYX_RUNTIME_CWASM=/runtime.cwasm cargo build --locked --release --target aarch64-unknown-linux-gnu \
      -p agentic-server --features embedded-code-interpreter \
 && install -Dm755 target/aarch64-unknown-linux-gnu/release/agentic-server /out/agentic-server \
 && aarch64-linux-gnu-strip /out/agentic-server

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates libssl3 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/agentic-server /usr/local/bin/agentic-server
# Its conversations, in /data/agentic-api on the node, which belongs to this user.
USER 10001:10001
ENV AGENTIC_API_HOME=/var/lib/agentic-api
WORKDIR /var/lib/agentic-api
ENTRYPOINT ["/usr/local/bin/agentic-server"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
