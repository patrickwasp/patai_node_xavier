# The CUDA runtime the GPU services share: Ubuntu 20.04, and of CUDA 11.4 only the runtime and cuBLAS. A node downloads
# it once, and each service's image adds only its own binaries. The NVIDIA runtime mounts the driver, libcuda and the
# Tegra libraries from the OS, so those are only named here, where the loader looks.
FROM nvcr.io/nvidia/l4t-cuda:11.4.19-devel@sha256:cff51f4845ff298faa3e41e42f1f19d037bafdb354af5fcf04c93fe981317d02 AS cuda

FROM ubuntu:20.04@sha256:8feb4d8ca5354def3d8fce243717141ce31e2c428701f6682bd2fafe15388214
COPY --from=cuda /usr/local/cuda/targets/aarch64-linux/lib/libcudart.so.11.0 \
     /usr/local/cuda/targets/aarch64-linux/lib/libcublas.so.11 \
     /usr/local/cuda/targets/aarch64-linux/lib/libcublasLt.so.11 /usr/local/cuda/lib64/
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgomp1 \
 && rm -rf /var/lib/apt/lists/* \
 && echo /usr/local/cuda/lib64 > /etc/ld.so.conf.d/cuda.conf \
 && printf '/usr/lib/aarch64-linux-gnu/tegra\n/usr/lib/aarch64-linux-gnu/tegra-egl\n' > /etc/ld.so.conf.d/nvidia-tegra.conf \
 && ldconfig
# What the NVIDIA runtime looks for before it mounts anything.
ENV NVIDIA_VISIBLE_DEVICES=all NVIDIA_DRIVER_CAPABILITIES=all
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
