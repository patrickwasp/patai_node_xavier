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

# NumPy and pandas compiled to WebAssembly, as shared libraries that Eryx links into its runtime. Nobody publishes
# them for CPython 3.14 on WASI, so they're built here the way dicej/wasi-wheels does (docs/research/interpreter-
# packages.md): wasi-sdk 27; dicej's CPython 3.14 fork built for WASI with dynamic linking, for its headers, its
# sysconfig and a libpython3.14.so to link against; NumPy at the commit dicej's build is of; then pandas.
FROM --platform=$BUILDPLATFORM python:3.14-slim-trixie@sha256:9aaa474097d094040649f2162a627c0049b408f52138d980916788d80f04802f AS wasm

ARG BUILDARCH
ARG CPYTHON_COMMIT=267f0e225186719674344c06ccc3aab68b621ee1
ARG NUMPY_COMMIT=7bc18034031f32e5d03bb646c472dabd1623e9d5
ARG PANDAS_VERSION=v2.3.3
ARG PANDAS_COMMIT=9c8bc3e55188c8aff37207a74f1dd144980b8874
RUN apt-get update \
 && apt-get install -y --no-install-recommends git curl ca-certificates build-essential pkg-config zlib1g-dev libffi-dev \
 && rm -rf /var/lib/apt/lists/*
RUN case "$BUILDARCH" in \
      amd64) arch=x86_64 sum=b7d4d944c88503e4f21d84af07ac293e3440b1b6210bfd7fe78e0afd92c23bc2 ;; \
      arm64) arch=arm64 sum=4cf4c553c4640e63e780442146f87d83fdff5737f988c06a6e3b2f0228e37665 ;; \
      *) exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/wasi-sdk.tgz "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-27/wasi-sdk-27.0-$arch-linux.tar.gz" \
 && echo "$sum  /tmp/wasi-sdk.tgz" | sha256sum -c - \
 && mkdir /opt/wasi-sdk && tar -xzf /tmp/wasi-sdk.tgz -C /opt/wasi-sdk --strip-components=1 && rm /tmp/wasi-sdk.tgz
ENV WASI_SDK_PATH=/opt/wasi-sdk
RUN git init -q /wasm/cpython && cd /wasm/cpython \
 && git fetch -q --depth 1 https://github.com/dicej/cpython "$CPYTHON_COMMIT" && git checkout -q FETCH_HEAD
WORKDIR /wasm/cpython/builddir/build
RUN ../../configure -q --prefix="$PWD/install" && make -s -j6
WORKDIR /wasm/cpython/builddir/wasi
RUN CONFIG_SITE=../../Tools/wasm/wasi/config.site-wasm32-wasi CFLAGS=-fPIC ../../Tools/wasm/wasi-env ../../configure -q -C \
      --host=wasm32-unknown-wasip2 --build="$(../../config.guess)" --with-build-python="$PWD/../build/python" \
      --prefix="$PWD/install" --enable-wasm-dynamic-linking --enable-ipv6 --disable-test-modules \
 && make -s -j6 build_all install \
 && "$WASI_SDK_PATH/bin/clang" --target=wasm32-wasip2 -shared -o install/lib/libpython3.14.so \
      -Wl,--whole-archive install/lib/libpython3.14.a -Wl,--no-whole-archive \
      Modules/_hacl/libHacl_HMAC.a Modules/_hacl/libHacl_Hash_BLAKE2.a Modules/_hacl/libHacl_Hash_MD5.a \
      Modules/_hacl/libHacl_Hash_SHA1.a Modules/_hacl/libHacl_Hash_SHA2.a Modules/_hacl/libHacl_Hash_SHA3.a \
      Modules/_decimal/libmpdec/libmpdec.a Modules/expat/libexpat.a \
      -lwasi-emulated-signal -lwasi-emulated-getpid -lwasi-emulated-process-clocks -ldl
ENV CROSS_PREFIX=/wasm/cpython/builddir/wasi/install
# What a setuptools build for the guest is given: the target's sysconfig through the build machine's Python 3.14.
COPY wasm-env.sh /wasm/env.sh
# Their sources fixed for WASI: a getter NumPy defines with the wrong signature, and pandas' imports of ctypes and mmap.
COPY wasm-packages.py /wasm/packages.py
RUN git init -q /wasm/numpy && cd /wasm/numpy \
 && git fetch -q --depth 1 https://github.com/numpy/numpy "$NUMPY_COMMIT" && git checkout -q FETCH_HEAD \
 && git submodule update -q --init --depth 1 \
 && python3.14 -I /wasm/packages.py numpy /wasm/numpy
RUN python3.14 -m venv /wasm/venv && /wasm/venv/bin/pip install -q cython==3.0.12 setuptools==71.1.0
RUN cd /wasm/numpy && . /wasm/env.sh && python setup.py -q build --disable-optimization -j 6
# pandas compiles against the guest's NumPy, its headers and its .pxd files, through a stand-in that only answers
# get_include(): the build machine's NumPy would give it x86's sizes.
RUN mkdir -p /wasm/numpy-for-pandas/numpy/include \
 && cp /wasm/numpy/numpy/*.pxd /wasm/numpy-for-pandas/numpy/ \
 && cp -a /wasm/numpy/numpy/core/include/numpy /wasm/numpy-for-pandas/numpy/include/ \
 && find /wasm/numpy/build -path '*/include/numpy/*.h' -exec cp {} /wasm/numpy-for-pandas/numpy/include/numpy/ \; \
 && test -f /wasm/numpy-for-pandas/numpy/include/numpy/_numpyconfig.h \
 && test -f /wasm/numpy-for-pandas/numpy/include/numpy/__multiarray_api.h \
 && printf '__version__ = "1.26.0b1"\n\ndef get_include():\n    return "/wasm/numpy-for-pandas/numpy/include"\n' \
      > /wasm/numpy-for-pandas/numpy/__init__.py
# The release's tag rather than its sdist, which has no setup.py; its version is from the tag.
RUN git init -q /wasm/pandas && cd /wasm/pandas \
 && git fetch -q --depth 1 https://github.com/pandas-dev/pandas "refs/tags/$PANDAS_VERSION:refs/tags/$PANDAS_VERSION" \
 && git checkout -q "$PANDAS_VERSION" && test "$(git rev-parse HEAD)" = "$PANDAS_COMMIT" \
 && python3.14 -I /wasm/packages.py pandas /wasm/pandas \
 && /wasm/venv/bin/pip install -q versioneer==0.29 wheel==0.45.1
# wasi-sdk's C++ has no exceptions: a catch is dropped, so a throw can only end the program. pandas' one C++ extension,
# its rolling windows, links __cxa_throw; this archive gives it one that aborts, which fails that call. Only what
# needs it takes it from the archive.
COPY wasm-cxa.c /wasm/cxa.c
RUN "$WASI_SDK_PATH/bin/clang" --target=wasm32-wasip2 -fPIC -O2 -c /wasm/cxa.c -o /wasm/cxa.o \
 && "$WASI_SDK_PATH/bin/ar" rcs /wasm/libpatai_cxa.a /wasm/cxa.o
RUN cd /wasm/pandas && . /wasm/env.sh && export PYTHONPATH="/wasm/numpy-for-pandas:$PYTHONPATH" \
      LDFLAGS="$LDFLAGS -L/wasm -lpatai_cxa" \
 && python setup.py -q build -j 4
# Without their tests, which nothing imports: a third of pandas.
RUN mkdir -p /out && cp -a /wasm/numpy/build/lib.*/numpy /wasm/pandas/build/lib.*/pandas /out/ \
 && find /out -type d -name tests -prune -exec rm -rf {} +
FROM --platform=$BUILDPLATFORM scratch AS wasm-out
COPY --from=wasm /out /

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
# SQLite's temporary files in memory: it can't make them on WASI.
COPY _patai_sqlite.py /site-packages/_patai_sqlite.py

# Eryx's runtime pre-initialized here with the packages most calls use imported, so a call starts from that snapshot,
# then compiled for the Xavier with Cranelift's baseline settings for aarch64: a node's Carmel cores have every
# feature that assumes, and nothing is compiled for the build machine's.
RUN ERYX_PRECOMPILE_BOOTSTRAP=1 cargo run --locked --release -p patai-eryx-precompile -- \
      /site-packages /runtime.cwasm aarch64-unknown-linux-gnu \
      json csv sqlite3 _patai_sqlite re datetime decimal statistics collections math itertools zipfile \
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
