# Sourced before building a package's extensions for the guest (Dockerfile, wasm stage), as dicej/wasi-wheels does:
# setuptools runs on the build machine's Python 3.14 but reads the WASI build's sysconfig, compiles with wasi-sdk for
# wasm32-wasip2 and links each extension as a shared library against libpython3.14.so. __EMSCRIPTEN__ takes the code
# paths that suit a WebAssembly target with no signals, threads or setjmp.
. /wasm/venv/bin/activate
export CC="$WASI_SDK_PATH/bin/clang"
export CXX="$WASI_SDK_PATH/bin/clang++"
export AR="$WASI_SDK_PATH/bin/ar"
export RANLIB=true
export LDSHARED="$CC"
export PYTHONPATH="$CROSS_PREFIX/lib/python3.14"
export CFLAGS="--target=wasm32-wasip2 -I$CROSS_PREFIX/include/python3.14 -D__EMSCRIPTEN__=1 -DNPY_NO_SIGNAL"
export CXXFLAGS="--target=wasm32-wasip2 -I$CROSS_PREFIX/include/python3.14"
export LDFLAGS="--target=wasm32-wasip2 -shared $CROSS_PREFIX/lib/libpython3.14.so"
export _PYTHON_SYSCONFIGDATA_NAME=_sysconfigdata__wasi_wasm32-wasi
export NPY_DISABLE_SVML=1
export NPY_BLAS_ORDER=
export NPY_LAPACK_ORDER=
export NPY_NO_SIGNAL=1
