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
# patai-eryx-precompile, which links the packages' extensions into Eryx's runtime, pre-initializes it with the
# packages imported and compiles it for the Xavier.

# NumPy and pandas compiled to WebAssembly, as shared libraries that Eryx links into its runtime. Nobody publishes
# them for CPython 3.14 on WASI, so they're built here, each release's sdist cross-compiled with meson
# (docs/apps.md, The code interpreter): wasi-sdk 27, and dicej's CPython 3.14 fork built for WASI with dynamic
# linking, for its headers, its sysconfig and a libpython3.14.so to link against.
FROM --platform=$BUILDPLATFORM python:3.14-slim-trixie@sha256:9aaa474097d094040649f2162a627c0049b408f52138d980916788d80f04802f AS wasm

ARG BUILDARCH
ARG CPYTHON_COMMIT=267f0e225186719674344c06ccc3aab68b621ee1
ARG NUMPY_SDIST=https://files.pythonhosted.org/packages/4e/3a/8770c288c47f0b5c2a786d3c20e79f66fd245f2d42cd232d7cd5e00e83e5/numpy-2.5.5.tar.gz
ARG NUMPY_SHA256=5cddc243c75114ba845f01b1f87aca1572dbf7b6c2d88636ed3e1d2bccd7f1bf
ARG PANDAS_SDIST=https://files.pythonhosted.org/packages/17/ca/24c6bc92aca3d366146358e47f1d374c0e1a50763ef226fc71eff20a63fb/pandas-3.1.1.tar.gz
ARG PANDAS_SHA256=05fed3e3b7c99b226f160e29d7d6cb7ef39638c758cbfe42b3f24ba34f04b859
# Pillow and matplotlib's compiled parts, and the C libraries Pillow takes: zlib for PNG, libjpeg-turbo for JPEG.
ARG ZLIB_URL=https://github.com/madler/zlib/releases/download/v1.4/zlib-1.4.tar.gz
ARG ZLIB_SHA256=02dd159783677a749c04adbdd02b55b8a2bcc7e054f51b76b549199a916ad584
ARG JPEG_URL=https://github.com/libjpeg-turbo/libjpeg-turbo/releases/download/3.2.1/libjpeg-turbo-3.2.1.tar.gz
ARG JPEG_SHA256=c27ad72a68da151177929aa969ae773a2da5ac2af9ce946079314fdac8cece7f
ARG PILLOW_SDIST=https://files.pythonhosted.org/packages/ce/a4/9cc30690a394a8305e8be4c9036aa16d0dfddadd09079860b582f562496d/pillow-13.0.0.tar.gz
ARG PILLOW_SHA256=cdcd1ba258178f7356d74170c9a81ba4eab6a5d55748bc402a5a2a6fe6be3510
ARG KIWISOLVER_SDIST=https://files.pythonhosted.org/packages/ba/07/bd78e6a8fae171ea041ef5bba3ed21a003522fa088834b069b1909981f30/kiwisolver-1.5.1.tar.gz
ARG KIWISOLVER_SHA256=f1303ef2eec81262a4b708c3e858afe58d7c75ad91c1c05266eda7673369859a
ARG CONTOURPY_SDIST=https://files.pythonhosted.org/packages/83/5a/a55177dd22553a277388e8a1b3220e92de91bacb28356cdc73caa240121d/contourpy-1.4.0.tar.gz
ARG CONTOURPY_SHA256=20156f5a1ac4f8ce02656e39a61e82164a3d359796dc8026f75b062783d500e1
ARG MATPLOTLIB_SDIST=https://files.pythonhosted.org/packages/e7/c8/9aa712a0afb882649424dd8de8ad9aa6235e796e84c6052e8f6dc1598d0d/matplotlib-3.11.2.tar.gz
ARG MATPLOTLIB_SHA256=cec596316640f2b394b8f0daa0ea61a8eae82d017b620b9f202befb972a59ea4
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
# meson and its tools on the build machine's Python. It's given only the WASI build's sysconfig, so it builds with the
# guest's extension suffix and sizes while pip and meson run on the build machine's own library.
RUN python3.14 -m venv /wasm/venv \
 && /wasm/venv/bin/pip install -q meson-python==0.22.1 meson==1.12.1 Cython==3.3.1 ninja==1.13.2 versioneer==0.29 \
      wheel==0.48.0 build==1.6.1 cmake==4.4.5 setuptools==84.2.1 setuptools-scm==10.3.4 pybind11==3.1.0 cppy==1.3.1 \
 && mkdir /wasm/sysconfig && cp "$CROSS_PREFIX/lib/python3.14/_sysconfigdata__wasi_wasm32-wasi.py" /wasm/sysconfig/
# wasm-cross.ini says how to compile for the guest, through wasm-cc.sh. wasi-sdk's C++ has no exceptions: a catch is
# dropped, so a throw can only end the program; wasm-cxa.c's archive gives an extension that links one (NumPy's FFT and
# unique, pandas' rolling windows) a throw that aborts, which fails that call.
COPY wasm-cross.ini wasm-cc.sh wasm-cxa.c wasm-packages.py wasm-setjmp.h wasm-pybind11-config.sh /wasm/
RUN mkdir /wasm/bin && install -m 755 /wasm/wasm-cc.sh /wasm/bin/cc && install -m 755 /wasm/wasm-cc.sh /wasm/bin/c++ \
 && install -m 755 /wasm/wasm-pybind11-config.sh /wasm/bin/pybind11-config \
 && install -D -m 644 /wasm/wasm-setjmp.h /wasm/shim/include/setjmp.h \
 && "$WASI_SDK_PATH/bin/clang" --target=wasm32-wasip2 -fPIC -O2 -c /wasm/wasm-cxa.c -o /wasm/cxa.o \
 && "$WASI_SDK_PATH/bin/ar" rcs /wasm/libpatai_cxa.a /wasm/cxa.o
ENV PATH=/wasm/bin:/wasm/venv/bin:$PATH
# Each sdist checked against its pin and fixed for WASI (wasm-packages.py), then built into /out. NumPy without BLAS,
# threads or SIMD, none of which the guest has.
RUN curl -fsSL -o /tmp/numpy.tgz "$NUMPY_SDIST" && echo "$NUMPY_SHA256  /tmp/numpy.tgz" | sha256sum -c - \
 && mkdir /wasm/numpy && tar -xzf /tmp/numpy.tgz -C /wasm/numpy --strip-components=1 && rm /tmp/numpy.tgz \
 && python3.14 -I /wasm/wasm-packages.py numpy /wasm/numpy \
 && cd /wasm/numpy \
 && PYTHONPATH=/wasm/sysconfig _PYTHON_SYSCONFIGDATA_NAME=_sysconfigdata__wasi_wasm32-wasi python -m build --wheel \
      --no-isolation --skip-dependency-check --outdir /wasm/wheels -Csetup-args=--cross-file=/wasm/wasm-cross.ini \
      -Csetup-args=-Dblas=none -Csetup-args=-Dlapack=none -Csetup-args=-Dallow-noblas=true \
      -Csetup-args=-Ddisable-threading=true -Csetup-args=-Ddisable-optimization=true -Csetup-args=-Ddisable-svml=true \
      -Csetup-args=-Ddisable-highway=true -Csetup-args=-Ddisable-intel-sort=true \
      -Csetup-args=-Dcpu-baseline=none -Csetup-args=-Dcpu-dispatch=none -Ccompile-args=-j6 \
 && mkdir /out && cd /out && python3.14 -m zipfile -e /wasm/wheels/numpy-*.whl .
# pandas compiles against the guest's NumPy, its headers and its .pxd files, through a stand-in that only answers
# get_include(): the build machine's NumPy would give it x86's sizes.
RUN mkdir -p /wasm/numpy-for-pandas/numpy && cp /out/numpy/*.pxd /wasm/numpy-for-pandas/numpy/ \
 && printf 'def get_include():\n    return "/out/numpy/_core/include"\n' > /wasm/numpy-for-pandas/numpy/__init__.py \
 && test -f /out/numpy/_core/include/numpy/_numpyconfig.h \
 && curl -fsSL -o /tmp/pandas.tgz "$PANDAS_SDIST" && echo "$PANDAS_SHA256  /tmp/pandas.tgz" | sha256sum -c - \
 && mkdir /wasm/pandas && tar -xzf /tmp/pandas.tgz -C /wasm/pandas --strip-components=1 && rm /tmp/pandas.tgz \
 && python3.14 -I /wasm/wasm-packages.py pandas /wasm/pandas \
 && cd /wasm/pandas \
 && PYTHONPATH=/wasm/numpy-for-pandas:/wasm/sysconfig _PYTHON_SYSCONFIGDATA_NAME=_sysconfigdata__wasi_wasm32-wasi \
      python -m build --wheel --no-isolation --skip-dependency-check --outdir /wasm/wheels \
      -Csetup-args=--cross-file=/wasm/wasm-cross.ini -Ccompile-args=-j6 \
 && cd /out && python3.14 -m zipfile -e /wasm/wheels/pandas-*.whl .
# zlib and libjpeg-turbo, as static libraries Pillow's extensions take in, then Pillow, with nothing but them: no
# FreeType, which matplotlib has of its own, nor Tk. A setuptools build reads the WASI build's sysconfig too.
RUN curl -fsSL -o /tmp/zlib.tgz "$ZLIB_URL" && echo "$ZLIB_SHA256  /tmp/zlib.tgz" | sha256sum -c - \
 && mkdir /wasm/zlib && tar -xzf /tmp/zlib.tgz -C /wasm/zlib --strip-components=1 && cd /wasm/zlib \
 && CC="$WASI_SDK_PATH/bin/clang" AR="$WASI_SDK_PATH/bin/llvm-ar" RANLIB="$WASI_SDK_PATH/bin/llvm-ranlib" \
      CFLAGS="--target=wasm32-wasip2 -fPIC -O2" ./configure --static --prefix=/wasm/deps >/dev/null \
 && make -s -j6 libz.a && make -s install >/dev/null \
 && curl -fsSL -o /tmp/jpeg.tgz "$JPEG_URL" && echo "$JPEG_SHA256  /tmp/jpeg.tgz" | sha256sum -c - \
 && mkdir -p /wasm/jpeg/build && tar -xzf /tmp/jpeg.tgz -C /wasm/jpeg --strip-components=1 && cd /wasm/jpeg/build \
 && cmake -G Ninja -DCMAKE_TOOLCHAIN_FILE="$WASI_SDK_PATH/share/cmake/wasi-sdk-p2.cmake" -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_C_FLAGS="-fPIC -I/wasm/shim/include" -DWITH_SIMD=0 -DENABLE_SHARED=0 -DWITH_TURBOJPEG=0 -DWITH_TOOLS=0 \
      -DWITH_TESTS=0 -DCMAKE_POSITION_INDEPENDENT_CODE=1 -DCMAKE_INSTALL_PREFIX=/wasm/deps .. >/dev/null \
 && ninja >/dev/null && ninja install >/dev/null && rm /tmp/zlib.tgz /tmp/jpeg.tgz
RUN curl -fsSL -o /tmp/pillow.tgz "$PILLOW_SDIST" && echo "$PILLOW_SHA256  /tmp/pillow.tgz" | sha256sum -c - \
 && mkdir /wasm/pillow && tar -xzf /tmp/pillow.tgz -C /wasm/pillow --strip-components=1 && rm /tmp/pillow.tgz \
 && python3.14 -I /wasm/wasm-packages.py pillow /wasm/pillow && cd /wasm/pillow \
 && PYTHONPATH=/wasm/sysconfig _PYTHON_SYSCONFIGDATA_NAME=_sysconfigdata__wasi_wasm32-wasi \
      CC=/wasm/bin/cc LDSHARED="/wasm/bin/cc -shared" AR="$WASI_SDK_PATH/bin/llvm-ar" ZLIB_ROOT=/wasm/deps JPEG_ROOT=/wasm/deps \
      CFLAGS="--target=wasm32-wasip2 -fPIC -I/wasm/shim/include -I/wasm/deps/include" \
      LDFLAGS="--target=wasm32-wasip2 -L/wasm/deps/lib" \
      python -m build --wheel --no-isolation --skip-dependency-check --outdir /wasm/wheels -Cplatform-guessing=disable \
      -Czlib=enable -Cjpeg=enable -Ctiff=disable -Cfreetype=disable -Craqm=disable -Clcms=disable -Cwebp=disable \
      -Cjpeg2000=disable -Cimagequant=disable -Cxcb=disable -Cavif=disable \
 && cd /out && python3.14 -m zipfile -e /wasm/wheels/pillow-*.whl .
# matplotlib's compiled parts: kiwisolver with setuptools, contourpy with meson, and matplotlib, whose meson build
# fetches and builds FreeType, qhull, HarfBuzz, libraqm and SheenBidi of its own. pybind11, the newest, is fine as it
# is; what it can't do here is raise through C++ (wasm-packages.py).
RUN curl -fsSL -o /tmp/kiwi.tgz "$KIWISOLVER_SDIST" && echo "$KIWISOLVER_SHA256  /tmp/kiwi.tgz" | sha256sum -c - \
 && mkdir /wasm/kiwi && tar -xzf /tmp/kiwi.tgz -C /wasm/kiwi --strip-components=1 && rm /tmp/kiwi.tgz && cd /wasm/kiwi \
 && PYTHONPATH=/wasm/sysconfig _PYTHON_SYSCONFIGDATA_NAME=_sysconfigdata__wasi_wasm32-wasi \
      CC=/wasm/bin/cc CXX=/wasm/bin/c++ LDSHARED="/wasm/bin/cc -shared" LDCXXSHARED="/wasm/bin/c++ -shared" \
      CFLAGS="--target=wasm32-wasip2 -fPIC" CXXFLAGS="--target=wasm32-wasip2 -fPIC" LDFLAGS="--target=wasm32-wasip2" \
      python -m build --wheel --no-isolation --skip-dependency-check --outdir /wasm/wheels \
 && curl -fsSL -o /tmp/contourpy.tgz "$CONTOURPY_SDIST" && echo "$CONTOURPY_SHA256  /tmp/contourpy.tgz" | sha256sum -c - \
 && mkdir /wasm/contourpy && tar -xzf /tmp/contourpy.tgz -C /wasm/contourpy --strip-components=1 && rm /tmp/contourpy.tgz \
 && cd /wasm/contourpy \
 && PYTHONPATH=/wasm/sysconfig _PYTHON_SYSCONFIGDATA_NAME=_sysconfigdata__wasi_wasm32-wasi python -m build --wheel \
      --no-isolation --skip-dependency-check --outdir /wasm/wheels -Csetup-args=--cross-file=/wasm/wasm-cross.ini \
      -Csetup-args=-Dwerror=false -Ccompile-args=-j6 \
 && cd /out && for wheel in /wasm/wheels/kiwisolver-*.whl /wasm/wheels/contourpy-*.whl; do \
      python3.14 -m zipfile -e "$wheel" . || exit 1; done
RUN curl -fsSL -o /tmp/matplotlib.tgz "$MATPLOTLIB_SDIST" && echo "$MATPLOTLIB_SHA256  /tmp/matplotlib.tgz" | sha256sum -c - \
 && mkdir /wasm/matplotlib && tar -xzf /tmp/matplotlib.tgz -C /wasm/matplotlib --strip-components=1 \
 && rm /tmp/matplotlib.tgz && python3.14 -I /wasm/wasm-packages.py matplotlib /wasm/matplotlib && cd /wasm/matplotlib \
 && PYTHONPATH=/wasm/numpy-for-pandas:/wasm/sysconfig _PYTHON_SYSCONFIGDATA_NAME=_sysconfigdata__wasi_wasm32-wasi \
      python -m build --wheel --no-isolation --skip-dependency-check --outdir /wasm/wheels \
      -Csetup-args=--cross-file=/wasm/wasm-cross.ini -Csetup-args=-Dmacosx=false -Csetup-args=-Db_lto=false \
      -Csetup-args=-Dwerror=false -Ccompile-args=-j6 \
 && cd /out && python3.14 -m zipfile -e /wasm/wheels/matplotlib-*.whl .
# Without their tests, which nothing imports: half of each.
RUN find /out -type d -name tests -prune -exec rm -rf {} + && rm -rf /out/*.dist-info \
 && test "$(find /out -name '*.so' | wc -l)" -gt 70
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
# NumPy and pandas, built above. Their extensions are linked into the runtime below.
COPY --from=wasm /out/ /site-packages/

# Eryx's runtime with the packages' WebAssembly extensions linked in, pre-initialized here with the packages most calls
# use imported, so a call starts from that snapshot, then compiled for the Xavier with Cranelift's baseline settings for aarch64: a node's Carmel cores have every
# feature that assumes, and nothing is compiled for the build machine's.
RUN ERYX_PRECOMPILE_BOOTSTRAP=1 cargo run --locked --release -p patai-eryx-precompile -- \
      /site-packages /runtime.cwasm aarch64-unknown-linux-gnu \
      json csv sqlite3 _patai_sqlite re datetime decimal statistics collections math itertools zipfile \
      openpyxl xlsxwriter pypdf mammoth tabulate dateutil pytz bs4 markdown pygal sympy networkx numpy pandas \
      PIL PIL.Image PIL.ImageDraw matplotlib matplotlib.pyplot \
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
