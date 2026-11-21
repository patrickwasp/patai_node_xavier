"""patai: NumPy's and pandas' sources fixed for WASI's Python (Dockerfile, wasm stage). Each edit is checked, so a
source that changed fails the build.

  wasm-packages.py numpy <numpy source>    before building it
  wasm-packages.py pandas <pandas source>  before building it

NumPy: a getter CPython calls with (self, closure) is defined with self alone. Native code doesn't mind; WebAssembly
checks every indirect call's type and traps, which is what pickling any pandas object did. And np.save and np.load
write and read a file as a stream, as ndarray.tofile() needs dup(), which WASI doesn't have.

pandas: WASI's Python has no ctypes or mmap, which pandas imports at load for names it uses only on Windows or with
memory_map=True. ctypes is imported where it's used instead, and mmap is a stand-in that nothing is an instance of and
that can't be made, so memory_map=True fails with an OSError that says why.
"""
import pathlib
import sys

kind, root = sys.argv[1], pathlib.Path(sys.argv[2])


def edit(path, *pairs):
    file = root / path
    text = file.read_text()
    for old, new in pairs:
        if text.count(old) != 1:
            sys.exit("%s: expected %r once, found it %d times" % (path, old, text.count(old)))
        text = text.replace(old, new)
    file.write_text(text)


def numpy():
    meta = "numpy/core/src/multiarray/dtypemeta.c"
    for name in ("abstract", "legacy", "parametric", "is_numeric"):
        edit(meta, ("\ndtypemeta_get_%s(PyArray_DTypeMeta *self) {" % name,
                    "\ndtypemeta_get_%s(PyArray_DTypeMeta *self, void *NPY_UNUSED(ignored)) {" % name))
    edit("numpy/core/src/multiarray/array_method.c",
         ("\nboundarraymethod__supports_unaligned(PyBoundArrayMethodObject *self)\n",
          "\nboundarraymethod__supports_unaligned(PyBoundArrayMethodObject *self, void *NPY_UNUSED(ignored))\n"))
    # WASI has no dup(), which ndarray.tofile() and fromfile() need, so np.save and np.load use files as streams.
    edit("numpy/compat/py3k.py",
         ("def isfileobj(f):\n",
          "def isfileobj(f):\n    if sys.platform == 'wasi':  # patai: no dup(), so tofile() can't be used\n        return False\n"))
    print("numpy patched for WASI")


STAND_IN = '''"""patai: a stand-in for mmap, which WASI's Python doesn't have (workloads/agentic-api/wasm-packages.py)."""
import types


class _NoMmap:
    def __init__(self, *args, **kwargs):
        raise OSError("mmap isn't available in this sandbox: read the file without memory_map")


mmap = types.SimpleNamespace(mmap=_NoMmap, ACCESS_READ=1, ACCESS_WRITE=2, ACCESS_COPY=3)
'''


def pandas():
    (root / "pandas/_patai_wasi.py").write_text(STAND_IN)
    edit("pandas/errors/__init__.py",
         ("\nimport ctypes\n", "\n"),
         ("ctypes.WinError()", '__import__("ctypes").WinError()'))
    edit("pandas/core/interchange/from_dataframe.py",
         ("\nimport ctypes\n", "\n"),
         ("ctypes.cast(", '__import__("ctypes").cast('),
         ("ctypes.POINTER(", '__import__("ctypes").POINTER('))
    for path in ("pandas/io/common.py", "pandas/io/excel/_openpyxl.py"):
        edit(path, ("\nimport mmap\n", "\nfrom pandas._patai_wasi import mmap\n"))
    # The clipboard is imported only when it's used.
    left = [str(p) for p in (root / "pandas").rglob("*.py") if "tests" not in p.parts
            and p.relative_to(root).as_posix() != "pandas/io/clipboard/__init__.py"
            and any(line in ("import ctypes", "import mmap") for line in p.read_text().splitlines())]
    if left:
        sys.exit("still imported at load: %s" % left)
    print("pandas patched for WASI")


{"numpy": numpy, "pandas": pandas}[kind]()
