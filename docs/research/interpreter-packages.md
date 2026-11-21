# Packages for the code interpreter

Researched 2026-11-19, to give the agentic API's code interpreter (`apps.md`, The code interpreter) what other data
analysis sandboxes have, as far as its sandbox allows. What it got is in `workloads/agentic-api/wheels.txt`.

## What the others ship

| Sandbox | What's in it |
| --- | --- |
| ChatGPT | A Linux container with over 300 packages (a March 2026 snapshot): numpy, pandas, scipy, scikit-learn, statsmodels, xarray, torch, xgboost, matplotlib, seaborn, plotly, bokeh, folium, geopandas, openpyxl, xlsxwriter, python-docx, python-pptx, pdfplumber, pypdf, reportlab, Pillow, opencv, sympy, networkx. Files in and out |
| Claude's code execution tool | pandas, numpy, scipy, scikit-learn, statsmodels; matplotlib, seaborn; pyarrow, openpyxl, xlsxwriter, xlrd, Pillow, python-pptx, python-docx, pypdf, pdfplumber, pypdfium2, pdf2image, reportlab; sympy, mpmath; tqdm, python-dateutil, pytz, joblib. Files in from the Files API, out from an output folder. No network, no installs |
| Gemini's code execution | 37 packages, much the same core, with opencv, fpdf, tabulate, geopandas, tensorflow. matplotlib only for charts, 30 s a run, no installs |

The common core: pandas and numpy, a chart library, Excel, Word and PDF readers and writers, Pillow, and sympy.

## What Eryx can run

The guest is CPython 3.14 compiled for WASI. On a node, checked through the gateway:

- **There:** `sqlite3` (SQLite 3.51), `zlib`, `gzip`, `zipfile`, `csv`, `json`, `decimal`, XML (`pyexpat`), `hashlib`,
  `zoneinfo`.
- **Not there:** `bz2`, `lzma`, `ctypes`, `mmap`, `readline`, and time zone data until `tzdata` is added.

Eryx 0.15 loads two kinds of package:

- **Pure-Python wheels** work as they are, if nothing they import is missing above. xlrd imports `mmap`, so it's out.
  sympy imports `ctypes` only to learn a C long's size, and networkx imports `bz2` only to open `.bz2` files: the
  Dockerfile takes those lines out.
- **Compiled extensions** must be built for WASI, and Eryx links them into the runtime when it's pre-initialized.
  Build recipes exist for the hard ones: HubSpot's Boomslang (Apache-2.0) has numpy 2.4, pandas 3.0, matplotlib 3.9,
  Pillow 12.2 and lxml for CPython 3.14 with wasi-sdk 25, statically linked; dicej's wasi-wheels has numpy, pandas,
  regex and pyyaml. Only numpy is published as a build.
- **Out of reach on WASI:** scipy, scikit-learn and statsmodels, which need Fortran; OpenCV; DuckDB, polars and pyarrow.

Pyodide, which Chat runs in the browser, has most of these. Run on a server, inside Deno as pydantic's mcp-run-python
does, it had a critical sandbox escape in 2026 (PYSEC-2026-2628) through Pyodide's bridge to JavaScript. Eryx gives the
guest nothing it isn't handed, so it stays.

## In tiers

1. **Pure-Python, now.** openpyxl, xlsxwriter, pypdf, mammoth (reads Word), tabulate, python-dateutil, pytz, tzdata,
   beautifulsoup4, markdown, sympy, networkx, pygal (SVG charts), and what they need. 11 MiB of wheels, 54 MiB
   unpacked.
2. **Compiled.** NumPy 2.5.5 and pandas 3.1.1, done (2026-11-20): each release's sdist cross-compiled with meson on
   the PC as shared WASI libraries Eryx links, with a few fixes for WASI (`docs/apps.md`, The code interpreter). Next
   the same way: Pillow, whose `ImageDraw` draws boxes and text on an image, which covers annotation without OpenCV;
   lxml, for python-docx and python-pptx; matplotlib, for PNG charts.
3. **Not on WASI:** the rest. numpy covers basic statistics and fitting.

For charts a client shows, a Vega-Lite spec the code prints, which Chat would draw, looks better than matplotlib's
defaults and is interactive. pygal's SVG works now, as a file.

## Sources

- [Anthropic code execution tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/code-execution-tool)
- [Gemini code execution](https://ai.google.dev/gemini-api/docs/code-execution)
- [ChatGPT sandbox snapshot, March 2026](https://gist.github.com/hyle/5c240a6343b7d4f3401e27dc75f72420)
- [Eryx's packages guide](https://github.com/eryx-org/eryx/blob/main/book/src/guide/packages.md)
- [dicej/wasi-wheels](https://github.com/dicej/wasi-wheels/), [benbrandt/wasi-wheels](https://github.com/benbrandt/wasi-wheels/)
- [Boomslang's supported libraries](https://github.hubspot.com/boomslang/guide/supported-libraries.html),
  [HubSpot/boomslang](https://github.com/HubSpot/boomslang)
- [Packages in Pyodide](https://pyodide.readthedocs.io/en/stable/usage/packages-in-pyodide.html)
- [PYSEC-2026-2628, mcp-run-python](https://osv.dev/vulnerability/PYSEC-2026-2628)
