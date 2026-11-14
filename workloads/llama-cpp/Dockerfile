# llama.cpp for the Xavier: CUDA 11.4, sm_72. Built in NVIDIA's CUDA image, run on the shared CUDA base
# (workloads/cuda-base), so its own layer is only llama.cpp's binaries and libraries.
# The shared CUDA base, by digest; scripts/publish-workloads sets it. A node that built the base can pass its own.
ARG BASE=ghcr.io/patrickwasp/patai-cuda-base@sha256:ce9e04549c72be4abf6fae65a8edf0b4e59130ed0e4f8fe6d447b031fee87e99
FROM nvcr.io/nvidia/l4t-cuda:11.4.19-devel@sha256:cff51f4845ff298faa3e41e42f1f19d037bafdb354af5fcf04c93fe981317d02 AS build

ARG LLAMA_VERSION=v1.0.0

RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential git python3-pip \
 && pip3 install 'cmake<4' \
 && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 -b "$LLAMA_VERSION" https://github.com/ggml-org/llama.cpp /opt/llama.cpp
WORKDIR /opt/llama.cpp

# Carmel is ARMv8.2 with FP16 and no dotprod; say so rather than trust -mcpu=native under Docker.
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release \
      -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=72 \
      -DGGML_NATIVE=OFF -DGGML_CPU_ARM_ARCH=armv8.2-a+fp16 \
      -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_CURL=OFF \
 && cmake --build build -j"$(nproc)" --target llama-server llama-cli llama-bench \
 && mkdir -p /out/bin /out/lib \
 && cp build/bin/llama-server build/bin/llama-cli build/bin/llama-bench /out/bin/ \
 && cp -P build/bin/*.so* /out/lib/ \
 && strip --strip-unneeded /out/bin/* /out/lib/*.so*

FROM ${BASE}
COPY --from=build /out/bin/ /usr/local/bin/
COPY --from=build /out/lib/ /usr/local/lib/
RUN ldconfig
WORKDIR /models
ENTRYPOINT ["/usr/local/bin/llama-server"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
