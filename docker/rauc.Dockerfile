# RAUC from source: Ubuntu 20.04 ships 1.2, which has neither streaming nor verity bundles. Build it
# with --platform: arm64 goes into the rootfs, and `docker run` prints that install tree as a tar;
# amd64 signs bundles, as /out/usr/bin/rauc, which is why mksquashfs is here.
FROM ubuntu:20.04@sha256:8feb4d8ca5354def3d8fce243717141ce31e2c428701f6682bd2fafe15388214
ARG RAUC=1.15.2
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      gcc libc6-dev meson ninja-build pkg-config xz-utils linux-libc-dev \
      libglib2.0-dev libssl-dev libcurl4-openssl-dev libdbus-1-dev libjson-glib-dev \
      libfdisk-dev libnl-genl-3-dev squashfs-tools \
 && rm -rf /var/lib/apt/lists/*
COPY vendor/rauc/rauc-${RAUC}.tar.xz /src/
RUN tar -C /src -xJf /src/rauc-${RAUC}.tar.xz \
 && cd /src/rauc-${RAUC} \
 && meson setup build --prefix=/usr -Dtests=false \
      -Dsystemdunitdir=/usr/lib/systemd/system \
      -Ddbuspolicydir=/usr/share/dbus-1/system.d \
      -Ddbussystemservicedir=/usr/share/dbus-1/system-services \
      -Ddbusinterfacesdir=/usr/share/dbus-1/interfaces \
 && ninja -C build \
 && DESTDIR=/out ninja -C build install
CMD ["tar", "-C", "/out", "-cf", "-", "."]
