# syntax=docker/dockerfile:1
#
# Ubuntu 20.04 flashing host for the Jetson AGX Xavier on a Connect Tech Rogue (AGX101).
# NVIDIA Jetson Linux R35.6.2 and CTI-L4T AGX-35.6.2 both require an 18.04 or 20.04 host.
#
#   host    the host tools only (l4t_flash_prerequisites.sh plus console/USB tools)
#   kernel  CTI's kernel built from CTI's source for a node, packaged as CTI's own (docs/kernel.md)
#   l4t     host + Linux_for_Tegra with the sample rootfs and CTI BSP installed, with that kernel, ready to flash
#
# Build with scripts/build-image (it registers qemu-aarch64 first; apply_binaries needs it).
# docker/bin is copied last in each target so editing a tool doesn't rebuild the BSP layer.

ARG UBUNTU=ubuntu:20.04@sha256:8feb4d8ca5354def3d8fce243717141ce31e2c428701f6682bd2fafe15388214

FROM ${UBUNTU} AS base
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
# First block: Linux_for_Tegra/tools/l4t_flash_prerequisites.sh (R35.6.2), verbatim.
# Second: what NVIDIA's scripts assume a desktop Ubuntu already has (full perl for
# bootloader/*.pl, bc for odmfuse.sh, python3-cryptography for disk encryption), then what
# flash.sh, l4t_create_default_user.sh and scripts/ in this repo use.
RUN apt-get update && apt-get install -y --no-install-recommends \
      abootimg binfmt-support binutils cpp device-tree-compiler dosfstools lbzip2 \
      libxml2-utils nfs-kernel-server python3-yaml qemu-user-static sshpass udev \
      uuid-runtime whois openssl cpio rsync zstd lz4 \
      perl bc python3-cryptography \
      python3 python-is-python3 python3-serial bzip2 xz-utils e2fsprogs kmod parted \
      gdisk file sudo ca-certificates curl iproute2 iputils-ping openssh-client \
      usbutils picocom socat netcat-openbsd vim-common less \
 && rm -rf /var/lib/apt/lists/*

FROM base AS host
ENV USER=root
COPY docker/bin/ /usr/local/bin/

FROM base AS kernel
ARG CTI=AGX-35.6.2-V001 CTI_SOURCE=cti-l4t-src-agx-35.6.2-v001.tgz
# What building a kernel needs beside base's tools. The compiler is NVIDIA's, from vendor/.
RUN apt-get update && apt-get install -y --no-install-recommends make gcc libc6-dev bison flex libssl-dev \
 && rm -rf /var/lib/apt/lists/*
RUN --mount=type=bind,source=vendor,target=/vendor --mount=type=bind,source=docker/kernel,target=/kernel \
    /kernel/build /vendor/cti/${CTI_SOURCE} /vendor/cache/nvidia/bootlin/aarch64--glibc--stable-final.tar.gz \
      /vendor/cti/CTI-L4T-${CTI}.tgz /out

# Only what the kernel stage made, for scripts/build-kernel to take out.
FROM scratch AS kernel-files
COPY --from=kernel /out/ /

FROM base AS l4t
ARG L4T=35.6.2 CTI=AGX-35.6.2-V001
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier \
      org.opencontainers.image.description="Jetson Linux R${L4T} + CTI-L4T ${CTI} for the Rogue (AGX101), ready to flash"
WORKDIR /opt/l4t
# One layer, so the vendor tarballs are read from the build context and never stored.
# CTI's install.sh copies its kernel, DTBs, bootloader and flash configs over NVIDIA's,
# swaps in its own debs, then runs NVIDIA's apply_binaries.sh (chroot via qemu-aarch64).
# The kernel stage's build takes the place of CTI's kernel, DTBs and kernel debs first.
RUN --mount=type=bind,source=vendor,target=/vendor --mount=type=bind,from=kernel,source=/out,target=/kernel \
    tar -I lbzip2 -xpf /vendor/nvidia/l4t-r${L4T}/Jetson_Linux_R${L4T}_aarch64.tbz2 \
 && tar -I lbzip2 -xpf /vendor/cache/nvidia/l4t-r${L4T}/Tegra_Linux_Sample-Root-Filesystem_R${L4T}_aarch64.tbz2 \
      -C Linux_for_Tegra/rootfs \
 && tar -xzf /vendor/cti/CTI-L4T-${CTI}.tgz -C Linux_for_Tegra \
 && rm -r Linux_for_Tegra/CTI-L4T/kernel \
 && mkdir Linux_for_Tegra/CTI-L4T/kernel \
 && cp -r /kernel/Image /kernel/dtb /kernel/*.deb Linux_for_Tegra/CTI-L4T/kernel/ \
 && cd Linux_for_Tegra/CTI-L4T && ./install.sh \
 && test "$(cat ../rootfs/etc/cti/CTI-L4T.version)" = "${CTI}" \
 && cmp ../kernel/Image /kernel/Image && cmp ../rootfs/boot/Image /kernel/Image
# flash.sh and friends check $USER, not the uid, and Docker doesn't set it.
ENV USER=root
COPY docker/bin/ /usr/local/bin/
WORKDIR /opt/l4t/Linux_for_Tegra
