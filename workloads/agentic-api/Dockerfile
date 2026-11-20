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
# patai.patch, on top of the release (docs/apps.md, The code interpreter): Eryx 0.15 and Wasmtime 50; containers, a
# folder of files at /mnt/data and the Python state that carry from one call to the next, with OpenAI's Containers
# API; a worker killed without cgroup.kill, which the Xavier's 5.10 lacks; one core per run; and
# patai-eryx-precompile, which pre-initializes Eryx's runtime with the packages and compiles it for the Xavier.
FROM --platform=$BUILDPLATFORM rust:1.100.0-slim-trixie@sha256:6f14b2b4b278ec0d50aa1d69541c304ce0f95e0f6268487aa9924af1c2e9aebe AS build

ARG BUILDARCH
ARG AGENTIC_VERSION=v0.10.0
ARG AGENTIC_COMMIT=b0f491e024ff73339b16e6f83a301041a51257d7
# A PC with 14 GiB runs out of memory linking at more.
ARG CARGO_BUILD_JOBS=6
ENV CARGO_BUILD_JOBS=$CARGO_BUILD_JOBS

RUN if [ "$BUILDARCH" != arm64 ]; then dpkg --add-architecture arm64; fi \
 && apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl unzip pkg-config build-essential libssl-dev:arm64 \
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

# The packages, each checked against its pin, in the /site-packages the guest sees. Two lines go where they'd import
# what WASI's Python hasn't got: sympy asks ctypes only how big a C long is, and networkx opens .bz2 files.
COPY wheels.txt /wheels.txt
RUN mkdir /site-packages && grep -v '^#' /wheels.txt | while read -r url sha256; do \
      curl -fsSL -o /tmp/wheel.whl "$url" \
      && echo "$sha256  /tmp/wheel.whl" | sha256sum -c - \
      && unzip -qo /tmp/wheel.whl -d /site-packages || exit 1; \
    done \
 && rm /tmp/wheel.whl \
 && test -z "$(find /site-packages -name '*.so' -print -quit)" \
 && f=/site-packages/sympy/external/gmpy.py \
 && sed -i -e 's/^from ctypes import c_long, sizeof$//' \
      -e 's/^LONG_MAX = (1 << (8\*sizeof(c_long) - 1)) - 1$/LONG_MAX = (1 << 31) - 1  # patai: a C long on wasm32/' "$f" \
 && ! grep -q ctypes "$f" \
 && f=/site-packages/networkx/utils/decorators.py \
 && sed -i -e '/^import bz2$/d' -e '/^    ".bz2": bz2.BZ2File,$/d' "$f" \
 && ! grep -q bz2 "$f"

# Eryx's runtime pre-initialized here with the packages most calls use imported, so a call starts from that snapshot,
# then compiled for the Xavier with Cranelift's baseline settings for aarch64: a node's Carmel cores have every
# feature that assumes, and nothing is compiled for the build machine's.
RUN ERYX_PRECOMPILE_BOOTSTRAP=1 cargo run --locked --release -p patai-eryx-precompile -- \
      /site-packages /runtime.cwasm aarch64-unknown-linux-gnu \
      json csv sqlite3 re datetime decimal statistics collections math itertools zipfile \
      openpyxl xlsxwriter pypdf mammoth tabulate dateutil pytz bs4 markdown pygal sympy networkx \
 && rm -rf target/release

RUN ERYX_RUNTIME_CWASM=/runtime.cwasm cargo build --locked --release --target aarch64-unknown-linux-gnu \
      -p agentic-server --features embedded-code-interpreter \
 && install -Dm755 target/aarch64-unknown-linux-gnu/release/agentic-server /out/agentic-server \
 && aarch64-linux-gnu-strip /out/agentic-server

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates libssl3t64 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/agentic-server /usr/local/bin/agentic-server
# Root's, and read-only to the server and its workers, which mount it where the runtime was pre-initialized with it.
COPY --from=build /site-packages /opt/agentic-api/site-packages
# Its conversations and containers, in /data/agentic-api on the node, which belongs to this user.
USER 10001:10001
ENV AGENTIC_API_HOME=/var/lib/agentic-api
WORKDIR /var/lib/agentic-api
ENTRYPOINT ["/usr/local/bin/agentic-server"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
