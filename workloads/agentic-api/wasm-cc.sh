#!/bin/sh
# wasi-sdk's clang for meson (wasm-cross.ini), installed as /wasm/bin/cc and c++. meson takes wasm-ld for ELF's lld, so
# what wasm-ld hasn't got is dropped: archive groups, which change nothing here. A shared library, a package's extension,
# is linked against the WASI build's libpython3.14.so, for its Python symbols, and wasm-cxa.c's archive, for a C++
# throw that can only abort (wasi-sdk has no exceptions). A test program meson builds to check for a function isn't, as
# one can't link a shared library statically.
case "$0" in *c++) tool=clang++ ;; *) tool=clang ;; esac
shared=
for arg do
  shift
  case "$arg" in
    -Wl,--start-group|-Wl,--end-group) continue ;;
    -shared) shared=1 ;;
  esac
  set -- "$@" "$arg"
done
[ -z "$shared" ] || set -- "$@" /wasm/cpython/builddir/wasi/install/lib/libpython3.14.so /wasm/libpatai_cxa.a
exec "/opt/wasi-sdk/bin/$tool" "$@"
