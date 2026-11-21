"""patai: NumPy's and pandas' sources fixed for WASI's Python (Dockerfile, wasm stage). Each edit is checked, so a
source that changed fails the build.

  wasm-packages.py numpy <numpy source>    before building it
  wasm-packages.py pandas <pandas source>  before building it
  wasm-packages.py pillow <Pillow source>  before building it
  wasm-packages.py matplotlib <its source>  before building it

NumPy: np.save and np.load write and read a file as a stream, as ndarray.tofile() needs dup(), which WASI doesn't
have.

pandas: WASI's Python has no ctypes or mmap, which pandas imports at load for names it uses only on Windows, with
memory_map=True or to read a big CSV in threads. ctypes is imported where it's used instead, and mmap is a stand-in
that nothing is an instance of and that can't be made, so memory_map=True fails with an OSError that says why. And
read_csv uses one thread on WASI, as it does on Emscripten: a guest can't start one.

Pillow: its Tk extension, which needs dlopen and a Tk the guest hasn't got, isn't built.

matplotlib: its C++ is bound with pybind11, which turns a Python error into a C++ throw, and wasi-sdk has no exceptions
in a shared library, so a throw aborts the run. ft2font's module __getattr__, which only gives names deprecated in
3.10, threw for every other name, as an import's hasattr asks: it's left out, so Python raises AttributeError itself.
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
    edit("numpy/lib/_format_impl.py",
         ("\nimport pickle\n", "\nimport pickle\nimport sys\n"),
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
    (root / "pandas/compat/_patai_wasi.py").write_text(STAND_IN)
    edit("pandas/errors/__init__.py",
         ("\nimport ctypes\n", "\n"),
         ("ctypes.WinError()", '__import__("ctypes").WinError()'))
    edit("pandas/core/interchange/from_dataframe.py",
         ("\nimport ctypes\n", "\n"),
         ("ctypes.cast(", '__import__("ctypes").cast('),
         ("ctypes.POINTER(", '__import__("ctypes").POINTER('))
    for path in ("pandas/io/common.py", "pandas/io/excel/_openpyxl.py", "pandas/io/parsers/readers.py"):
        edit(path, ("\nimport mmap\n", "\nfrom pandas.compat._patai_wasi import mmap\n"))
    edit("pandas/io/parsers/readers.py",
         ('    if sys.platform == "emscripten":\n', '    if sys.platform in ("emscripten", "wasi"):\n'))
    # The clipboard is imported only when it's used.
    left = [str(p) for p in (root / "pandas").rglob("*.py") if "tests" not in p.parts
            and p.relative_to(root).as_posix() != "pandas/io/clipboard/__init__.py"
            and any(line in ("import ctypes", "import mmap") for line in p.read_text().splitlines())]
    if left:
        sys.exit("still imported at load: %s" % left)
    print("pandas patched for WASI")


def pillow():
    edit("setup.py",
         ('        self._update_extension("PIL._imagingtk", tk_libs)\n',
          '        self._remove_extension("PIL._imagingtk")  # patai: no Tk, nor dlopen, in the guest\n'))
    print("Pillow patched for WASI")


def matplotlib():
    edit("src/ft2font_wrapper.cpp",
         ('    m.def("__getattr__", ft2font__getattr__);\n',
          '    // patai: no __getattr__, which throws for any name but a deprecated one (wasm-packages.py).\n'))
    print("matplotlib patched for WASI")


{"numpy": numpy, "pandas": pandas, "pillow": pillow, "matplotlib": matplotlib}[kind]()
