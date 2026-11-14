# Shared settings for scripts/. Source it; don't run it.
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

l4t_version=35.6.2
cti_release=V001
cti_version=AGX-${l4t_version}-${cti_release}
board_config=cti/xavier/rogue/base   # CTI profile for a commercial AGX Xavier on Rogue

registry=ghcr.io/patrickwasp/patai_node_xavier
image="$registry:l4t-${l4t_version}-cti-${cti_release,,}"
image_latest="$registry:latest"
image_host="$registry:host"
ubuntu_base=cache/ubuntu/ubuntu-base-20.04.5-base-arm64.tar.gz   # in vendor/MANIFEST


# Recovery-mode USB ID of a Jetson AGX Xavier (APX).
recovery_usb_id=0955:7019

# Run a command in the flashing image with the host's /dev (USB re-enumerates during a
# flash, and flash.sh uses loop devices), host networking and the repo at /repo.
l4t_run() {
  local tty=()
  [[ -t 0 && -t 1 ]] && tty=(-t)
  docker run --rm -i "${tty[@]}" --privileged --network host \
    -v /dev:/dev -v "$repo:/repo" "$image" "$@"
}

in_recovery() { lsusb -d "$recovery_usb_id" >/dev/null 2>&1; }

# Where scripts/build-product leaves a version's images.
product_dir() { realpath -m "${PATAI_OUT:-$repo/out}/$1"; }

# RAUC built for a platform (arm64 for the rootfs, amd64 to sign bundles). The repo's
# .dockerignore keeps the rest of vendor/ out of the normal build context, so this sends its own.
rauc_version=$(sed -n 's/^ARG RAUC=//p' "$repo/docker/rauc.Dockerfile")
rauc_image() { echo "patai/rauc:$rauc_version-$1"; }
build_rauc() {
  tar -C "$repo" -cf - docker/rauc.Dockerfile "vendor/rauc/rauc-$rauc_version.tar.xz" \
    | docker build -q --platform "linux/$1" -t "$(rauc_image "$1")" -f docker/rauc.Dockerfile -
}


# The console for the unit. As with RAUC, this sends its own build context.
console_image=patai/console:build
build_console() {
  tar -C "$repo" --exclude=console/ui/node_modules --exclude=console/ui/.next --exclude=console/ui/out \
    --exclude='console/internal/web/dist/*' -cf - docker/console.Dockerfile console workloads apps \
    | docker build -q -t "$console_image" -f docker/console.Dockerfile -
}
