# stable-diffusion.cpp for the Xavier: CUDA 11.4, sm_72. Built in NVIDIA's CUDA image, run on the shared CUDA base
# (workloads/cuda-base), so its own layer is only its two programs.
# The shared CUDA base, by digest; scripts/publish-workloads sets it. A node that built the base can pass its own.
ARG BASE=ghcr.io/patrickwasp/patai-cuda-base@sha256:ce9e04549c72be4abf6fae65a8edf0b4e59130ed0e4f8fe6d447b031fee87e99
FROM nvcr.io/nvidia/l4t-cuda:11.4.19-devel@sha256:cff51f4845ff298faa3e41e42f1f19d037bafdb354af5fcf04c93fe981317d02 AS build

ARG SDCPP_VERSION=master-1091-b9d7176

RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential git python3-pip \
 && pip3 install 'cmake<4' \
 && rm -rf /var/lib/apt/lists/*

# Only the ggml submodule: CUTLASS's kernels are for newer GPUs, WebP/WebM output isn't needed, and the server's web
# page needs Node to build.
RUN git clone --depth 1 -b "$SDCPP_VERSION" https://github.com/leejet/stable-diffusion.cpp /opt/sd.cpp \
 && git -C /opt/sd.cpp submodule update --init --depth 1 ggml
WORKDIR /opt/sd.cpp

# CUDA 11.4 has no __float22bfloat162_rn below sm_80: build each half with __float2bfloat16, which it has. The
# grep fails the build if the source has changed and the patch no longer applies.
RUN sed -i 's/__float22bfloat162_rn(make_float2(\(swiglu([^)]*)\), \(swiglu([^)]*)\)))/CUDABFloat162{__float2bfloat16(\1), __float2bfloat16(\2)}/' \
      src/core/cuda/bf16.cu \
 && ! grep -q __float22bfloat162_rn src/core/cuda/bf16.cu

# Carmel is ARMv8.2 with FP16 and no dotprod, as for llama.cpp.
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release \
      -DSD_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=72 -DSD_CUDA_CUTLASS_PATH= \
      -DSD_WEBP=OFF -DSD_WEBM=OFF -DSD_SERVER_BUILD_FRONTEND=OFF \
      -DGGML_NATIVE=OFF -DGGML_CPU_ARM_ARCH=armv8.2-a+fp16 \
 && cmake --build build -j"$(nproc)" --target sd-cli sd-server \
 && mkdir -p /out/bin /out/lib \
 && cp build/bin/sd-cli build/bin/sd-server /out/bin/ \
 && find build -name '*.so*' -exec cp -P {} /out/lib/ \; \
 && strip --strip-unneeded /out/bin/*

FROM ${BASE}
COPY --from=build /out/bin/ /usr/local/bin/
COPY --from=build /out/lib/ /usr/local/lib/
RUN ldconfig
WORKDIR /models
ENTRYPOINT ["/usr/local/bin/sd-server"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
