#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-fetch, against a real local HTTP server and real curl, with a
stand-in for docker. It has to pass on a unit's Python 3.8:

    python3 tests/patai-fetch.py
    scripts/ssh python3 - < tests/patai-fetch.py    (with the script in /usr/local/sbin)
"""
import hashlib
import http.server
import importlib.machinery
import importlib.util
import io
import json
import os
import re
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__)) if "__file__" in globals() else "/nonexistent"
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-fetch")
if not os.path.exists(SCRIPT):
    SCRIPT = "/usr/local/sbin/patai-fetch"

DOCKER = """#!/bin/sh
echo "docker $*" >> "$STUB_LOG"
case "$1 $2" in
  "image inspect")
    [ -e "$STUB_IMAGE" ] || exit 1
    cat "$STUB_IMAGE"
    exit 0
    ;;
esac
case "$1" in
  pull)
    [ ! -e "$STUB_PULL_FAILS" ] || { echo "toomanyrequests: retry later"; exit 1; }
    echo "1: Pulling from patrickwasp/patai-llama"
    echo "aaaaaaaaaaaa: Pulling fs layer"
    echo "aaaaaaaaaaaa: Download complete"
    echo "bbbbbbbbbbbb: Already exists"
    echo "aaaaaaaaaaaa: Pull complete"
    cat "$STUB_PULLS_AS" > "$STUB_PULLED"
    ;;
  tag)
    [ -e "$STUB_PULLED" ] && cp "$STUB_PULLED" "$STUB_IMAGE"
    ;;
esac
"""
LAYERS = [{"digest": "sha256:" + "a" * 64, "size": 700}, {"digest": "sha256:" + "b" * 64, "size": 300}]

LIBRARY = """#!/bin/sh
if [ "$1" = show ]; then cat "$STUB_LIBRARY"; exit 0; fi
echo "patai-workload $*" >> "$STUB_LOG"
"""

MODEL = b"model bytes " * 2000
def gguf(kind=0, shape=(8,), offset=0, alignment=None, data=None):
    """A small GGUF file: one metadata value, one tensor of type kind, then its data, aligned."""
    def string(text):
        raw = text.encode()
        return len(raw).to_bytes(8, "little") + raw

    pairs = string("general.name") + (8).to_bytes(4, "little") + string("test")
    if alignment:
        pairs += string("general.alignment") + (4).to_bytes(4, "little") + alignment.to_bytes(4, "little")
    out = b"GGUF" + (3).to_bytes(4, "little") + (1).to_bytes(8, "little") + (1 + bool(alignment)).to_bytes(8, "little")
    out += pairs + string("weight") + len(shape).to_bytes(4, "little")
    out += b"".join(dim.to_bytes(8, "little") for dim in shape) + kind.to_bytes(4, "little") + offset.to_bytes(8, "little")
    out += b"\x00" * (-len(out) % (alignment or 32))
    return out + (b"\x01" * 4 * shape[0] if data is None else data)


GGUF = gguf()


def safetensors(tensors=1, extra=b""):
    """A small safetensors file: a header that accounts for each tensor's bytes, then the bytes."""
    header, offset = {"__metadata__": {"format": "pt"}}, 0
    for i in range(tensors):
        header[f"t{i}"] = {"dtype": "F16", "shape": [4], "data_offsets": [offset, offset + 8]}
        offset += 8
    raw = json.dumps(header).encode()
    return len(raw).to_bytes(8, "little") + raw + b"\x00" * offset + extra


def sha(data):
    return hashlib.sha256(data).hexdigest()


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = root = self.tmp.name
        for name in ("www/models", "up/models", "bin", "recipes/llama", "data/models", "data/workloads", "run"):
            os.makedirs(os.path.join(root, name))
        self.log = os.path.join(root, "calls")
        os.environ.update(STUB_LOG=self.log, STUB_IMAGE=f"{root}/image-id", STUB_PULLS_AS=f"{root}/pulls-as",
                          STUB_PULLED=f"{root}/pulled", STUB_PULL_FAILS=f"{root}/pull-fails",
                          STUB_LIBRARY=f"{root}/library.json")
        self.write("bin/docker", DOCKER)
        os.chmod(f"{root}/bin/docker", 0o755)
        self.write("bin/patai-workload", LIBRARY)
        os.chmod(f"{root}/bin/patai-workload", 0o755)
        os.makedirs(f"{root}/data/.uploads")
        self.path = os.environ["PATH"]
        os.environ["PATH"] = f"{root}/bin:{self.path}"
        self.write("pulls-as", "sha256:good\n")

        # The mirror stands in for Wasupu's bucket, holding each model whole by its hash, and upstream for a model's
        # own address.
        self.mirror_server, self.mirror_url = self.serve("www")
        self.upstream_server, self.upstream = self.serve("up")

        self.unpacked = 0
        self.ref = "ghcr.io/patrickwasp/patai-llama:1"
        self.have, self.own, self.use = None, [], ""
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", dir="")
        self.write("recipes/llama/compose.yaml", f"services:\n  server:\n    image: {self.ref}\n")
        self.write("up/models/m.gguf", MODEL)

        loader = importlib.machinery.SourceFileLoader("patai_fetch", SCRIPT)
        spec = importlib.util.spec_from_loader("patai_fetch", loader)
        self.f = importlib.util.module_from_spec(spec)
        loader.exec_module(self.f)
        f = self.f
        f.RECIPES = f"{root}/recipes"
        f.DATA = f"{root}/data"
        f.MODELS = f"{root}/data/models"
        f.WORKLOADS = f"{root}/data/workloads"
        f.PARTIAL = f"{root}/data/workloads/.partial"
        f.STATUS = f"{root}/run/fetch.json"
        f.LOCK = f"{root}/run/fetch.lock"
        f.WATCH_S = 0.01
        # Small ranges, so a small model is many.
        f.RANGE = 1000
        f.WORKLOAD = f"{root}/bin/patai-workload"
        f.UPLOAD = f"{root}/data/.uploads/model"
        f.os.path.ismount = lambda path: not os.path.exists(f"{root}/unmounted")

    def tearDown(self):
        for server in (self.mirror_server, self.upstream_server):
            server.shutdown()
            server.server_close()
        os.environ["PATH"] = self.path
        self.tmp.cleanup()

    def serve(self, folder, ranges=True):
        """Files from a folder, answering a range request with just that range, as GitHub's and Hugging Face's CDNs
        do, unless ranges is False."""
        path = os.path.join(self.root, folder)

        class Handler(http.server.SimpleHTTPRequestHandler):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, directory=path, **kwargs)

            def send_head(self):
                found = re.fullmatch(r"bytes=(\d+)-(\d+)", self.headers.get("Range", ""))
                where = self.translate_path(self.path)
                if not found or not ranges or not os.path.isfile(where):
                    return super().send_head()
                with open(where, "rb") as f:
                    data = f.read()
                start, end = int(found[1]), min(int(found[2]), len(data) - 1)
                self.send_response(206)
                self.send_header("Content-Range", f"bytes {start}-{end}/{len(data)}")
                self.send_header("Content-Length", str(end - start + 1))
                self.end_headers()
                return io.BytesIO(data[start:end + 1])

            def log_message(self, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        return server, f"http://127.0.0.1:{server.server_address[1]}"

    def write(self, name, data):
        path = os.path.join(self.root, name)
        with open(path, "wb" if isinstance(data, bytes) else "w") as f:
            f.write(data)
        return path

    def recipe(self, model, url, dir="", more=(), files=(), mirror=False):
        """The manifest, with this model, its files, (name, bytes, url) each, and any more: (name, bytes, url, dir).
        With mirror, the model comes from the mirror first."""
        models = [{"name": "m.gguf", "dir": dir, "sha256": sha(model), "size": len(model), "url": url,
                   **({"mirror": f"{self.mirror_url}/models/{sha(model)}/m.gguf"} if mirror else {}),
                   "files": [{"name": name, "sha256": sha(data), "size": len(data), "url": address}
                             for name, data, address in files]}]
        models += [{"name": name, "dir": inner, "sha256": sha(data), "size": len(data), "url": address}
                   for name, data, address, inner in more]
        image = {"ref": self.ref, "digest": "sha256:" + "d" * 64, "id": "sha256:good", "size": 1000,
                 "unpacked": self.unpacked, "layers": LAYERS}
        self.write("recipes/llama/manifest.json", json.dumps({"name": "llama", "image": image, "models": models,
                                                              "model": {"dir": "", "formats": ["gguf"]}}))
        self.models = [model["name"] for model in models]
        self.library()

    def library(self):
        """What patai-workload says the stack keeps: every pinned model unless a test says otherwise."""
        have = self.models + [record["name"] for record in self.own] if self.have is None else self.have
        self.write("library.json", json.dumps({"models": {"llama": {"have": have, "use": self.use, "own": self.own}}}))

    def mirrored(self, data):
        """data in the mirror, where the bucket would hold the model pinned."""
        os.makedirs(f"{self.root}/www/models/{sha(MODEL)}", exist_ok=True)
        self.write(f"www/models/{sha(MODEL)}/m.gguf", data)

    def read(self, name):
        with open(os.path.join(self.root, name)) as f:
            return f.read()

    def run_it(self, *args):
        argv, stdout = sys.argv, sys.stdout
        sys.argv = ["patai-fetch", *(args or ["llama"])]
        sys.stdout = open(os.devnull, "w")
        try:
            self.f.main()
        except SystemExit as stop:
            return stop.code
        finally:
            sys.stdout.close()
            sys.argv, sys.stdout = argv, stdout

    def status(self):
        with open(self.f.STATUS) as f:
            return json.load(f)

    def model(self, inner=""):
        path = os.path.join(self.f.MODELS, "llama", inner, "m.gguf")
        with open(path, "rb") as f:
            return f.read()

    def calls(self):
        try:
            with open(self.log) as f:
                return f.read()
        except OSError:
            return ""


class Fetching(Base):
    def test_everything_from_nothing(self):
        self.assertIsNone(self.run_it())
        self.assertEqual(self.status()["step"], "ready")
        self.assertEqual(self.model(), MODEL)
        self.assertIn(f"docker pull ghcr.io/patrickwasp/patai-llama@sha256:{'d' * 64}", self.calls())
        self.assertIn(f"docker tag ghcr.io/patrickwasp/patai-llama@sha256:{'d' * 64} {self.ref}", self.calls())
        self.assertEqual(os.listdir(self.f.PARTIAL), [])

    def test_nothing_to_do(self):
        self.run_it()
        os.remove(self.log)
        self.assertIsNone(self.run_it())
        self.assertNotIn("docker pull", self.calls())

    def test_a_checked_model_isnt_hashed_at_each_boot(self):
        self.run_it()
        calls = []
        real = self.f.digest
        self.f.digest = lambda *a: calls.append(a) or real(*a)
        self.run_it()
        self.assertEqual(calls, [])

    def test_takes_the_model_from_the_mirror_first(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", mirror=True)
        self.mirrored(MODEL)
        os.remove(f"{self.root}/up/models/m.gguf")
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_mirror_without_it_falls_through_to_the_address(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", mirror=True)
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_directory_in_the_recipe(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", dir="checkpoints")
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model("checkpoints"), MODEL)

    def test_says_how_far_it_got(self):
        seen = []
        real = self.f.say
        self.f.say = lambda stack, step, total=0, done=0, *a, **k: (seen.append((step, total, done)),
                                                                     real(stack, step, total, done, *a, **k))
        self.run_it()
        steps = [step for number, (step, _, _) in enumerate(seen) if number == 0 or seen[number - 1][0] != step]
        self.assertEqual(steps, ["image", "model", "checking", "ready"])
        # The pulled layer and the one already there: all of it, and never more.
        self.assertEqual(max(done for step, _, done in seen if step == "image"), 1000)
        self.assertEqual(max(done for step, _, done in seen if step == "model"), len(MODEL))


    def test_several_models(self):
        other = b"other model " * 1000
        self.write("up/models/n.gguf", other)
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", more=[("n.gguf", other, f"{self.upstream}/models/n.gguf", "more")])
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)
        with open(os.path.join(self.f.MODELS, "llama", "more", "n.gguf"), "rb") as f:
            self.assertEqual(f.read(), other)
        with open(f"{self.root}/data/workloads/.llama-fetched") as f:
            self.assertEqual(len(f.read().splitlines()), 2)
        calls = []
        real = self.f.digest
        self.f.digest = lambda *a: calls.append(a) or real(*a)
        self.assertIsNone(self.run_it())
        self.assertEqual(calls, [])

    def test_a_models_files_come_with_it(self):
        projector = b"projector " * 1000
        self.write("up/models/p.gguf", projector)
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", dir="more",
                    files=[("m-mmproj.gguf", projector, f"{self.upstream}/models/p.gguf")])
        self.assertIsNone(self.run_it())
        with open(os.path.join(self.f.MODELS, "llama", "more", "m-mmproj.gguf"), "rb") as f:
            self.assertEqual(f.read(), projector)
        with open(f"{self.root}/data/workloads/.llama-fetched") as f:
            self.assertEqual(sorted(line.split()[0] for line in f.read().splitlines()), sorted([sha(MODEL), sha(projector)]))
        calls = []
        real = self.f.digest
        self.f.digest = lambda *a: calls.append(a) or real(*a)
        self.assertIsNone(self.run_it())
        self.assertEqual(calls, [])

    def test_a_bad_file_fails_its_model(self):
        self.write("up/models/p.gguf", b"not the projector")
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", files=[("m-mmproj.gguf", b"projector", f"{self.upstream}/models/p.gguf")])
        self.assertTrue(self.run_it())
        self.assertEqual(self.status()["step"], "failed")
        self.assertFalse(os.path.exists(os.path.join(self.f.MODELS, "llama", "m-mmproj.gguf")))

    def test_an_odd_file_name_in_the_manifest(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", files=[("../p.gguf", b"projector", f"{self.upstream}/models/p.gguf")])
        self.assertTrue(self.run_it())
        self.assertIn("odd file", self.status()["detail"])

    def test_a_model_no_longer_pinned_loses_its_line(self):
        other = b"other model " * 1000
        self.write("up/models/n.gguf", other)
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", more=[("n.gguf", other, f"{self.upstream}/models/n.gguf", "")])
        self.run_it()
        newer = b"newer model " * 2000
        self.write("up/models/m.gguf", newer)
        self.recipe(newer, f"{self.upstream}/models/m.gguf")
        self.assertIsNone(self.run_it())
        with open(f"{self.root}/data/workloads/.llama-fetched") as f:
            lines = f.read().splitlines()
        self.assertEqual([line.split()[0] for line in lines], [sha(newer)])



class Resuming(Base):
    def partial(self, data, done, total=None):
        os.makedirs(self.f.PARTIAL, exist_ok=True)
        self.write("data/workloads/.partial/m.gguf", data)
        self.write("data/workloads/.partial/m.gguf.progress",
                   json.dumps({"total": total or len(MODEL), "range": self.f.RANGE, "done": done, "bytes": 0}))

    def test_it_carries_on_from_the_ranges_it_had(self):
        # What the server has now is wrong where the partial is done, so only carrying on gives the model.
        half = len(MODEL) // 2
        self.partial(MODEL[:half] + b"\0" * (len(MODEL) - half), list(range(half // self.f.RANGE)))
        self.write("up/models/m.gguf", b"x" * half + MODEL[half:])
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_it_carries_on_from_the_mirror(self):
        half = len(MODEL) // 2
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", mirror=True)
        self.mirrored(b"x" * half + MODEL[half:])
        os.remove(f"{self.root}/up/models/m.gguf")
        self.partial(MODEL[:half] + b"\0" * (len(MODEL) - half), list(range(half // self.f.RANGE)))
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_what_the_mirror_gave_carries_on_from_the_address(self):
        # A mirror that stops partway leaves its ranges, which the model's own address finishes.
        half = len(MODEL) // 2
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", mirror=True)
        self.mirrored(MODEL[:half])
        self.write("up/models/m.gguf", b"x" * half + MODEL[half:])
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_record_for_another_size_starts_again(self):
        self.partial(b"x" * len(MODEL), list(range(5)), total=len(MODEL) + 1)
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_bad_partial_is_thrown_away_for_the_next_source(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", mirror=True)
        self.partial(b"x" * len(MODEL), list(range(len(MODEL) // self.f.RANGE)))
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_server_that_sends_no_ranges(self):
        server, address = self.serve("up", ranges=False)
        self.recipe(MODEL, f"{address}/models/m.gguf")
        self.assertIsNotNone(self.run_it())
        server.shutdown()
        server.server_close()
        self.assertIn("doesn't send part of a file", self.status()["detail"])


class Refusing(Base):
    def test_an_image_that_pulls_as_something_else(self):
        self.write("pulls-as", "sha256:other\n")
        self.assertIsNotNone(self.run_it())
        self.assertIn("isn't the image", self.status()["detail"])

    def test_a_failed_pull_says_why(self):
        self.write("pull-fails", "")
        self.assertIsNotNone(self.run_it())
        self.assertIn("toomanyrequests", self.status()["detail"])
        os.remove(f"{self.root}/pull-fails")
        self.assertIsNone(self.run_it())

    def test_an_image_not_from_the_registry(self):
        self.ref = "docker.io/someone/llama:1"
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf")
        self.assertIsNotNone(self.run_it())
        self.assertIn("odd image", self.status()["detail"])
        self.assertNotIn("docker pull", self.calls())

    def test_a_bad_mirror_doesnt_stop_it_getting_the_real_one(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", mirror=True)
        self.mirrored(b"x" * len(MODEL))
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_bad_model_everywhere_leaves_nothing(self):
        self.write("up/models/m.gguf", b"x" * len(MODEL))
        self.assertIsNotNone(self.run_it())
        self.assertIn("doesn't match", self.status()["detail"])
        self.assertEqual(os.listdir(self.f.PARTIAL), [])
        self.assertFalse(os.path.exists(os.path.join(self.f.MODELS, "llama", "m.gguf")))

    def test_a_dead_server_says_so(self):
        self.recipe(MODEL, "http://127.0.0.1:9/m.gguf")
        self.assertIsNotNone(self.run_it())
        self.assertIn("127.0.0.1", self.status()["detail"])
        self.assertNotIn("curl", self.status()["detail"])

    def test_a_disk_without_room(self):
        real = self.f.shutil.disk_usage
        self.f.shutil.disk_usage = lambda path: type("D", (), {"free": self.f.KEEP_FREE})
        try:
            self.assertIsNotNone(self.run_it())
        finally:
            self.f.shutil.disk_usage = real
        self.assertIn("GB free", self.status()["detail"])

    def test_room_to_unpack_it_too(self):
        self.unpacked = 10 ** 12
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf")
        self.assertIsNotNone(self.run_it())
        self.assertIn("GB free", self.status()["detail"])
        self.assertNotIn("docker pull", self.calls())

    def test_an_odd_name_in_the_manifest(self):
        for dir in ("../elsewhere", "a/b", ".hidden"):
            self.recipe(MODEL, f"{self.upstream}/models/m.gguf", dir=dir)
            self.assertIsNotNone(self.run_it())
            self.assertIn("odd file", self.status()["detail"])

    def test_a_broken_manifest(self):
        self.write("recipes/llama/manifest.json", '{"name": "llama", "image": {}}')
        self.assertIsNotNone(self.run_it())
        self.assertIn("manifest can't be read", self.status()["detail"])
        os.remove(f"{self.root}/recipes/llama/manifest.json")
        self.assertIsNotNone(self.run_it())

    def test_no_disk(self):
        self.write("unmounted", "")
        self.assertIsNotNone(self.run_it())
        self.assertIn("isn't mounted", self.status()["detail"])

    def test_no_such_stack(self):
        self.assertIsNotNone(self.run_it("nothing-here"))
        self.assertIsNotNone(self.run_it("../etc"))
        # Every one is checked before any is fetched.
        self.assertIsNotNone(self.run_it("llama", "../etc"))
        self.assertEqual(self.calls(), "")

    def test_saying_a_stack_wouldnt_start(self):
        self.run_it("--failed", "llama", "its container wouldn't start")
        self.assertEqual(self.status()["step"], "failed")


class Healing(Base):
    def test_a_damaged_model_is_replaced(self):
        self.run_it()
        path = os.path.join(self.f.MODELS, "llama", "m.gguf")
        os.remove(f"{self.root}/data/workloads/.llama-fetched")
        with open(path, "r+b") as f:
            f.write(b"Y")
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_model_kept_from_before_is_checked_not_fetched(self):
        os.makedirs(os.path.join(self.f.MODELS, "llama"))
        self.write("data/models/llama/m.gguf", MODEL)
        os.remove(f"{self.root}/up/models/m.gguf")
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_change_of_the_same_size_is_noticed(self):
        self.run_it()
        path = os.path.join(self.f.MODELS, "llama", "m.gguf")
        before = os.stat(path)
        time.sleep(0.02)
        with open(path, "r+b") as f:
            f.write(b"Y")
        os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_file_changing_while_it_is_read_proves_nothing(self):
        path = self.write("changing", MODEL)
        real = hashlib.sha256

        class Changing:
            def __init__(self):
                self.inner = real()

            def update(self, chunk):
                self.inner.update(chunk)
                time.sleep(0.02)
                with open(path, "r+b") as f:
                    f.write(b"Y")

            def hexdigest(self):
                return self.inner.hexdigest()

        self.f.hashlib = type("H", (), {"sha256": Changing})
        self.assertEqual(self.f.digest(path), ("", ""))

    def test_a_marker_for_another_model_means_nothing(self):
        self.run_it()
        newer = b"newer model " * 2000
        self.write("up/models/m.gguf", newer)
        self.recipe(newer, f"{self.upstream}/models/m.gguf")
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), newer)

    def test_an_empty_marker_file_means_checking_again(self):
        self.run_it()
        self.write("data/workloads/.llama-fetched", "")
        os.remove(f"{self.root}/up/models/m.gguf")
        self.assertIsNone(self.run_it())


class Bounded(Base):
    def test_a_server_that_sends_more_than_asked_is_held_to_it(self):
        class Endless(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(206)
                self.end_headers()
                try:
                    while True:
                        self.wfile.write(b"x" * 65536)
                        self.wfile.flush()
                except OSError:
                    pass

            def log_message(self, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Endless)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        os.remove(f"{self.root}/up/models/m.gguf")
        self.recipe(MODEL, f"http://127.0.0.1:{server.server_address[1]}/m.gguf")
        self.write("image-id", "sha256:good\n")
        self.assertIsNotNone(self.run_it())
        server.shutdown()
        server.server_close()
        self.assertIn("doesn't match", self.status()["detail"])
        self.assertEqual(os.listdir(self.f.PARTIAL), [])
        self.assertEqual(os.path.getsize(f"{self.root}/data/models/llama/m.gguf") if os.path.exists(
            f"{self.root}/data/models/llama/m.gguf") else 0, 0)


class Choosing(Base):
    def test_only_the_models_it_keeps(self):
        other = b"other model " * 1000
        self.write("up/models/n.gguf", other)
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", more=[("n.gguf", other, f"{self.upstream}/models/n.gguf", "")])
        self.have = ["n.gguf"]
        self.library()
        self.assertIsNone(self.run_it())
        self.assertFalse(os.path.exists(os.path.join(self.f.MODELS, "llama", "m.gguf")))
        with open(os.path.join(self.f.MODELS, "llama", "n.gguf"), "rb") as f:
            self.assertEqual(f.read(), other)

    def test_a_model_it_doesnt_keep_brings_no_files(self):
        other = b"other model " * 1000
        self.write("up/models/n.gguf", other)
        self.write("up/models/p.gguf", b"projector")
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", more=[("n.gguf", other, f"{self.upstream}/models/n.gguf", "")],
                    files=[("m-mmproj.gguf", b"projector", f"{self.upstream}/models/p.gguf")])
        self.have = ["n.gguf"]
        self.library()
        self.assertIsNone(self.run_it())
        self.assertEqual(sorted(os.listdir(os.path.join(self.f.MODELS, "llama"))), ["n.gguf"])

    def test_an_owners_model_comes_back_from_its_address(self):
        self.write("up/models/mine.gguf", GGUF)
        self.own = [{"name": "mine.gguf", "sha256": sha(GGUF), "size": len(GGUF), "url": f"{self.upstream}/models/mine.gguf"}]
        self.library()
        self.assertIsNone(self.run_it())
        with open(os.path.join(self.f.MODELS, "llama", "mine.gguf"), "rb") as f:
            self.assertEqual(f.read(), GGUF)

    def test_an_owners_model_has_to_match_what_was_added(self):
        self.write("up/models/mine.gguf", GGUF + b"changed")
        self.own = [{"name": "mine.gguf", "sha256": sha(GGUF), "size": len(GGUF) + 7, "url": f"{self.upstream}/models/mine.gguf"}]
        self.use = "mine.gguf"
        self.library()
        self.assertIsNotNone(self.run_it())
        self.assertIn("doesn't match what was added", self.status()["detail"])

    def test_an_owners_model_that_doesnt_check_out_leaves_the_folder(self):
        # As if something changed it, and it was uploaded, so there's nowhere to get it again.
        self.own = [{"name": "mine.gguf", "sha256": sha(GGUF), "size": len(GGUF), "url": ""}]
        self.library()
        os.makedirs(os.path.join(self.f.MODELS, "llama"))
        self.write("data/models/llama/mine.gguf", b"x" * len(GGUF))
        self.assertIsNone(self.run_it())
        self.assertFalse(os.path.exists(os.path.join(self.f.MODELS, "llama", "mine.gguf")))
        self.assertEqual(self.model(), MODEL)

    def test_an_upload_thats_gone_can_wait_unless_it_runs(self):
        self.own = [{"name": "gone.gguf", "sha256": sha(GGUF), "size": len(GGUF), "url": ""}]
        self.library()
        self.assertIsNone(self.run_it())
        self.use = "gone.gguf"
        self.library()
        self.assertIsNotNone(self.run_it())
        self.assertIn("add it again", self.status()["detail"])


class Adding(Base):
    def add(self, name, url=None, upload=None):
        if upload is not None:
            self.write("data/.uploads/model", upload)
        return self.run_it("--add-upload", "llama", name) if url is None else self.run_it("--add", "llama", name, url)

    def placed(self, name):
        path = os.path.join(self.f.MODELS, "llama", name)
        if not os.path.exists(path):
            return None
        with open(path, "rb") as f:
            return f.read()

    def test_from_an_address(self):
        self.write("up/models/mine.gguf", GGUF)
        self.assertIsNone(self.add("mine.gguf", f"{self.upstream}/models/mine.gguf"))
        self.assertEqual(self.placed("mine.gguf"), GGUF)
        self.assertIn(f"patai-workload model record llama mine.gguf {sha(GGUF)} {len(GGUF)} {self.upstream}/models/mine.gguf",
                      self.calls())
        self.assertEqual(os.listdir(self.f.PARTIAL), [])

    def test_from_an_upload(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf")
        self.write("recipes/llama/manifest.json", self.read("recipes/llama/manifest.json").replace('"gguf"]', '"gguf", "safetensors"]'))
        weights = safetensors(3)
        self.assertIsNone(self.add("mine.safetensors", upload=weights))
        self.assertEqual(self.placed("mine.safetensors"), weights)
        self.assertIn(f"model record llama mine.safetensors {sha(weights)} {len(weights)} -", self.calls())
        self.assertFalse(os.path.exists(self.f.UPLOAD))

    def test_a_pickle_called_safetensors_is_refused(self):
        self.write("recipes/llama/manifest.json", self.read("recipes/llama/manifest.json").replace('"gguf"]', '"safetensors"]'))
        for bad in (b"\x80\x04\x95" + b"pickle" * 100, safetensors(2, extra=b"tail"), b"x" * 7):
            self.assertIsNotNone(self.add("bad.safetensors", upload=bad))
            self.assertIn("isn't a safetensors file", self.status()["detail"])
            self.assertIsNone(self.placed("bad.safetensors"))
        self.assertNotIn("model record", self.calls())
        self.assertEqual(os.listdir(self.f.PARTIAL), [])

    def test_only_the_formats_the_app_takes(self):
        self.assertIsNotNone(self.add("weights.gguf", upload=safetensors(1)))
        self.assertIn("isn't a GGUF file", self.status()["detail"])
        for name in ("model.ckpt", "model.safetensors", "../x.gguf", "a b.gguf"):
            self.assertIsNotNone(self.add(name, upload=GGUF))
            self.assertIn("name isn't one", self.status()["detail"])

    def test_something_else_at_an_address(self):
        self.write("up/models/page.gguf", b"<html>" * 100)
        self.assertIsNotNone(self.add("page.gguf", f"{self.upstream}/models/page.gguf"))
        self.assertIsNone(self.placed("page.gguf"))

    def test_a_link_in_place_of_the_upload(self):
        victim = self.write("victim.gguf", GGUF)
        os.symlink(victim, self.f.UPLOAD)
        self.assertIsNotNone(self.add("mine.gguf"))
        self.assertIn("didn't arrive", self.status()["detail"])
        self.assertIsNone(self.placed("mine.gguf"))

    def test_headers(self):
        def check(data):
            path = self.write("probe", data)
            handle = os.open(path, os.O_RDONLY)
            try:
                return self.f.model_format(handle)
            finally:
                os.close(handle)
        self.assertEqual(check(GGUF), "gguf")
        self.assertEqual(check(gguf(kind=12, shape=(256, 2), data=b"\x00" * 288)), "gguf")
        # MXFP4, gpt-oss's: 17 bytes for each 32 values.
        self.assertEqual(check(gguf(kind=39, shape=(64,), data=b"\x00" * 34)), "gguf")
        self.assertIsNone(check(gguf(kind=39, shape=(64,), data=b"\x00" * 33)))
        self.assertEqual(check(gguf(alignment=64)), "gguf")
        self.assertIsNone(check(b"GGUF" + (9).to_bytes(4, "little") + b"x" * 20))
        # Codex's examples: a GGUF that's only its first eight bytes, and a type, a place or a size that's wrong.
        self.assertIsNone(check(b"GGUF" + (3).to_bytes(4, "little")))
        self.assertIsNone(check(gguf(kind=99)))
        self.assertIsNone(check(gguf(offset=4)))
        self.assertIsNone(check(gguf(data=b"\x00" * 16)))
        self.assertIsNone(check(gguf(kind=12, shape=(100,), data=b"\x00" * 288)))
        self.assertIsNone(check(gguf(alignment=48)))
        self.assertIsNone(check(GGUF[:40]))
        # Codex's second look: more than llama.cpp's four dimensions, a huge one behind a zero.
        self.assertIsNone(check(gguf(shape=(8, 1, 1, 1, 1), data=b"\x00" * 32)))
        self.assertIsNone(check(gguf(shape=(8, 2 ** 64 - 1, 0), data=b"\x00" * 32)))
        self.assertEqual(check(gguf(shape=(8, 1, 1, 1), data=b"\x00" * 32)), "gguf")
        self.assertEqual(check(safetensors(2)), "safetensors")
        self.assertIsNone(check((10).to_bytes(8, "little") + b"[1,2,3,4,5]"))
        self.assertIsNone(check((2 << 40).to_bytes(8, "little") + b"{}"))

        def tensors(entries, data):
            raw = json.dumps(entries).encode()
            return len(raw).to_bytes(8, "little") + raw + data

        for entries, data in (
                ({"t": {"dtype": "F16", "shape": [1], "data_offsets": [8, 4]}}, b"\x00" * 8),
                ({"t": {"dtype": "NOT_A_TYPE", "shape": [4], "data_offsets": [0, 4]}}, b"junk"),
                ({"t": {"dtype": "F16", "data_offsets": [0, 4]}}, b"junk"),
                ({"t": {"dtype": "F16", "shape": [4], "data_offsets": [0, 4]}}, b"junk"),
                ({"t": {"dtype": "U8", "shape": [2], "data_offsets": [2, 4]}}, b"junk"),
                ({"a": {"dtype": "U8", "shape": [4], "data_offsets": [0, 4]},
                  "b": {"dtype": "U8", "shape": [2], "data_offsets": [2, 4]}}, b"junk"),
                ({"t": {"dtype": "U8", "shape": [4], "data_offsets": [0, 4], "extra": 1}}, b"junk"),
                ({"__metadata__": {"x": 1}, "t": {"dtype": "U8", "shape": [4], "data_offsets": [0, 4]}}, b"junk"),
                ({"t": {"dtype": [], "shape": [4], "data_offsets": [0, 4]}}, b"junk"),
                ({"t": {"dtype": {}, "shape": [4], "data_offsets": [0, 4]}}, b"junk")):
            self.assertIsNone(check(tensors(entries, data)), entries)
        self.assertEqual(check(tensors({"t": {"dtype": "U8", "shape": [2, 2], "data_offsets": [0, 4]}}, b"ok!!")),
                         "safetensors")
        deep = b"[" * 100000 + b"]" * 100000
        self.assertIsNone(check(len(deep).to_bytes(8, "little") + deep))

    def test_nested_metadata_arrays(self):
        def string(text):
            return len(text).to_bytes(8, "little") + text
        nested = b"GGUF" + (3).to_bytes(4, "little") + (0).to_bytes(8, "little") + (1).to_bytes(8, "little")
        nested += string(b"x") + (9).to_bytes(4, "little") + (9).to_bytes(4, "little") + (1).to_bytes(8, "little")
        nested += (4).to_bytes(4, "little") + (1).to_bytes(8, "little") + (7).to_bytes(4, "little")
        path = self.write("probe", nested + b"\x00" * 32)
        handle = os.open(path, os.O_RDONLY)
        try:
            self.assertIsNone(self.f.model_format(handle))
        finally:
            os.close(handle)


class Links(Base):
    """A workload can write to /data/models. Nothing it leaves there may steer root."""

    def setUp(self):
        super().setUp()
        self.victim = self.write("victim", "precious\n")

    def intact(self):
        with open(self.victim) as f:
            self.assertEqual(f.read(), "precious\n")

    def test_a_link_at_the_model_is_replaced_not_followed(self):
        os.makedirs(os.path.join(self.f.MODELS, "llama"))
        os.symlink(self.victim, os.path.join(self.f.MODELS, "llama", "m.gguf"))
        self.assertIsNone(self.run_it())
        self.intact()
        self.assertFalse(os.path.islink(os.path.join(self.f.MODELS, "llama", "m.gguf")))
        self.assertEqual(self.model(), MODEL)

    def test_a_link_to_a_good_copy_isnt_accepted(self):
        good = self.write("good-copy", MODEL)
        os.makedirs(os.path.join(self.f.MODELS, "llama"))
        os.symlink(good, os.path.join(self.f.MODELS, "llama", "m.gguf"))
        self.assertIsNone(self.run_it())
        self.assertFalse(os.path.islink(os.path.join(self.f.MODELS, "llama", "m.gguf")))

    def test_a_link_for_the_directory_is_refused(self):
        self.recipe(MODEL, f"{self.upstream}/models/m.gguf", dir="checkpoints")
        os.makedirs(os.path.join(self.f.MODELS, "llama"))
        elsewhere = os.path.join(self.root, "elsewhere")
        os.makedirs(elsewhere)
        os.symlink(elsewhere, os.path.join(self.f.MODELS, "llama", "checkpoints"))
        self.assertIsNotNone(self.run_it())
        self.assertIn("isn't a plain directory", self.status()["detail"])
        self.assertEqual(os.listdir(elsewhere), [])

    def test_a_fifo_cant_hang_it(self):
        fifo = os.path.join(self.root, "fifo")
        os.mkfifo(fifo)
        self.assertEqual(self.f.digest(fifo), ("", ""))

    def test_a_fifo_at_the_model_is_replaced(self):
        os.makedirs(os.path.join(self.f.MODELS, "llama"))
        os.mkfifo(os.path.join(self.f.MODELS, "llama", "m.gguf"))
        self.assertIsNone(self.run_it())
        self.assertEqual(self.model(), MODEL)

    def test_a_directory_at_the_model_is_a_reason_not_a_crash(self):
        os.makedirs(os.path.join(self.f.MODELS, "llama", "m.gguf"))
        self.assertIsNotNone(self.run_it())
        self.assertIn("can't be replaced", self.status()["detail"])


if __name__ == "__main__":
    unittest.main(argv=["patai-fetch"], verbosity=1)
