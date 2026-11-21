#!/bin/sh
# pybind11-config for meson building for the code interpreter's guest (Dockerfile, wasm stage), first on PATH:
# pybind11's own answers, but --includes without the build machine's Python headers, which it adds before the WASI
# build's and which say a long is 64 bits.
real=/wasm/venv/bin/pybind11-config
[ -x "$real" ] || real=/wasm/mvenv/bin/pybind11-config
case "$1" in
  --includes) "$real" --includes | tr ' ' '\n' | grep -v '/include/python3' | tr '\n' ' ' ;;
  *) exec "$real" "$@" ;;
esac
