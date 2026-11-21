/* patai: setjmp for a library compiled for the code interpreter's guest (Dockerfile, wasm stage). wasi-sdk has setjmp
   only with WebAssembly's exception handling, which the guest isn't built with. A library that sets a jump point to
   come back to after an error, as libjpeg's users do, gets one that never comes back: the error aborts the run, where
   native code would raise. Found before the WASI libc's own on the include path. */
#ifndef PATAI_WASM_SETJMP_H
#define PATAI_WASM_SETJMP_H

#include <stdlib.h>

typedef int jmp_buf[1];
typedef int sigjmp_buf[1];

#define setjmp(env) 0
#define _setjmp(env) 0
#define sigsetjmp(env, save) 0

static inline void patai_longjmp(void) {
    abort();
}
#define longjmp(env, value) patai_longjmp()
#define _longjmp(env, value) patai_longjmp()
#define siglongjmp(env, value) patai_longjmp()

#endif
