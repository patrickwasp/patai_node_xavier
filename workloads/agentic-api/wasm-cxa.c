/* The two C++ runtime calls a throw makes, for an extension built with wasi-sdk, which has no exceptions: there is
   nothing to unwind to, so a throw aborts (Dockerfile, wasm stage). */
#include <stddef.h>
#include <stdlib.h>

void *__cxa_allocate_exception(size_t size) {
    (void)size;
    abort();
}

void __cxa_throw(void *exception, void *type, void (*destructor)(void *)) {
    (void)exception;
    (void)type;
    (void)destructor;
    abort();
}
