#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-update, against a real local HTTP server and real curl, with
stand-ins for rauc, systemctl and patai-channel, whose own tests are console/internal/channel's. It has to pass
on a unit's Python 3.8:

    python3 tests/patai-update.py
    scripts/ssh python3 - < tests/patai-update.py    (with the script in /usr/local/sbin)
"""
import http.server
import importlib.machinery
import importlib.util
import json
import os
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__)) if "__file__" in globals() else "/nonexistent"
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-update")
if not os.path.exists(SCRIPT):
    SCRIPT = "/usr/local/sbin/patai-update"

STUB = """#!/bin/sh
echo "$(basename "$0") $*" >> "$STUB_LOG"
if [ "$*" = "is-active --quiet patai-reboot.timer patai-halt.timer" ]; then
  [ -e "$STUB_REBOOTING" ]
  exit $?
fi
if [ "$(basename "$0")" = rauc ]; then
  # What it was given, and what the console was told was downloading as it started.
  for last; do :; done
  [ -f "$last" ] && echo "bundle $(wc -c < "$last")" >> "$STUB_LOG"
  [ -f "$STUB_DOWNLOAD" ] && echo "download $(cat "$STUB_DOWNLOAD")" >> "$STUB_LOG"
fi
if [ "$(basename "$0")" = rauc ] && [ -e "$STUB_FAIL" ]; then
  # As RAUC 1.15 does: the reason on stdout, and only that it failed on stderr.
  echo "100% Installing failed."
  echo "LastError: the state on this unit can't be read"
  echo "rauc-Message: some logging" >&2
  echo "Installing \\`$*\\` failed" >&2
  exit 1
fi
exit 0
"""

# Stands in for patai-channel verify: a "signed" file here is {"signed": <payload>}, and anything else is refused
# the way the real one refuses, with its reason on stderr.
VERIFIER = """#!/usr/bin/env python3
import hashlib, json, sys
try:
    payload = json.load(sys.stdin)["signed"]
except Exception:
    sys.exit("its signature doesn't match")
payload["payload_sha256"] = hashlib.sha256(json.dumps(payload, sort_keys=True).encode()).hexdigest()
payload["signer"] = "test"
print(json.dumps(payload))
"""

HASH = "ab" * 32
BUNDLE = b"a signed bundle " * 1000


class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.server.requests.append(f"{self.headers.get('Host')}{self.path}")
        if self.path.startswith("/moved/"):
            # As GitHub answers for a release's file: somewhere else has it.
            self.send_response(302)
            self.send_header("Location", f"http://localhost:{self.server.server_address[1]}{self.path[6:]}")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if self.path.endswith("/busy.raucb"):
            # Asks to be left alone for an hour, which curl would wait out if it retried by itself.
            self.send_response(503)
            self.send_header("Retry-After", "3600")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if self.path.endswith("/flaky.raucb"):
            self.server.flaky = getattr(self.server, "flaky", 0) + 1
            if self.server.flaky == 1:
                self.send_error(500)
                return
            self.path = self.path.replace("/flaky.raucb", "/x.raucb")
        if self.path.endswith("/stalls.raucb"):
            # Says it's sending a bundle, sends a little of it, then nothing.
            self.send_response(200)
            self.send_header("Content-Length", str(len(BUNDLE)))
            self.end_headers()
            self.wfile.write(BUNDLE[:10])
            self.wfile.flush()
            time.sleep(8)
            return
        super().do_GET()


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = self.tmp.name
        self.www = os.path.join(root, "www")
        for name in ("www", "www/v2", "bin", "provision", "report", "rauc", "run", "data"):
            os.makedirs(os.path.join(root, name))
        self.log = os.path.join(root, "calls")
        self.fail_flag = os.path.join(root, "fail")
        os.environ["STUB_LOG"], os.environ["STUB_FAIL"] = self.log, self.fail_flag
        os.environ["STUB_REBOOTING"] = os.path.join(root, "rebooting")
        os.environ["STUB_DOWNLOAD"] = os.path.join(root, "run/patai/update-download.json")
        for name, text in (("rauc", STUB), ("systemctl", STUB), ("patai-channel", VERIFIER)):
            path = os.path.join(root, "bin", name)
            with open(path, "w") as f:
                f.write(text)
            os.chmod(path, 0o755)
        self.path = os.environ["PATH"]
        os.environ["PATH"] = os.path.join(root, "bin") + ":" + self.path

        www = self.www

        class Handler(Quiet):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, directory=www, **kwargs)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.requests = []
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.channel = f"http://127.0.0.1:{self.server.server_address[1]}"

        loader = importlib.machinery.SourceFileLoader("patai_update", SCRIPT)
        spec = importlib.util.spec_from_loader("patai_update", loader)
        self.u = importlib.util.module_from_spec(spec)
        loader.exec_module(self.u)
        u = self.u
        u.RELEASE = self.write("release", "v1\n")
        u.CHANNEL = os.path.join(root, "provision/update-channel")
        u.HOLD = os.path.join(root, "provision/update-hold")
        u.REBOOT = os.path.join(root, "provision/update-reboot")
        u.TRIED = os.path.join(root, "rauc/update-tried")
        u.RESULTS = os.path.join(root, "report")
        u.CHECK = os.path.join(root, "report/update-check.json")
        u.HISTORY = os.path.join(root, "report/update-history.json")
        u.BUNDLE = self.write("run/update.raucb", "bundle")
        u.BUNDLE_INFO = self.write("run/update.json", "{}")
        u.LOCK = os.path.join(root, "run/update.lock")
        u.CHANNEL_NAME = os.path.join(root, "provision/update-channel-name")
        u.FLOORS = os.path.join(root, "rauc/channel-floors.json")
        u.FLOORS_LOCK = os.path.join(root, "run/channel-floors.lock")
        u.BUILT = os.path.join(root, "built-at")
        u.KEYRING = os.path.join(root, "keyring.pem")
        u.VERIFY = os.path.join(root, "bin/patai-channel")
        u.DATA = os.path.join(root, "data")
        u.ON_DISK = os.path.join(root, "data/.patai-update")
        u.IN_MEMORY = os.path.join(root, "run/patai-update")
        u.DOWNLOAD = os.environ["STUB_DOWNLOAD"]
        u.MEMINFO = self.write("meminfo", "MemTotal: 32000000 kB\nMemAvailable: 30000000 kB\n")
        u.CURL_PATIENCE = ["--connect-timeout", "5", "--speed-limit", "1024", "--speed-time", "2"]
        u.BETWEEN_TRIES_S = 0.2
        with open(os.path.join(self.www, "v2/x.raucb"), "wb") as f:
            f.write(BUNDLE)
        self.downloaded = os.path.join(u.IN_MEMORY, "update.raucb")
        self.installed = f"rauc install --require-manifest-hash={HASH} {self.downloaded}"
        self.write("provision/update-channel", self.channel + "\n")
        self.write("provision/update-channel-name", "stable\n")
        self.sequence = 1762200000

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        os.environ["PATH"] = self.path
        self.tmp.cleanup()

    def write(self, name, text):
        path = os.path.join(self.tmp.name, name)
        with open(path, "w") as f:
            f.write(text)
        return path

    def offer(self, version="v2", bundle="v2/x.raucb", **changes):
        """A signed file, each one a second newer than the last unless it says its sequence."""
        if "sequence" not in changes:
            self.sequence += 1
        payload = {"compatible": "patai-node-xavier", "channel": "stable", "sequence": self.sequence,
                   "version": version, "bundle": bundle, "manifest_hash": HASH, "size": len(BUNDLE)}
        payload.update(changes)
        payload = {k: v for k, v in payload.items() if v is not None or k == "bundle"}
        self.write("www/latest.json", json.dumps({"signed": payload}))

    def floors(self):
        with open(self.u.FLOORS) as f:
            return json.load(f)

    def run_it(self, *args):
        argv = sys.argv
        sys.argv = ["patai-update", *args]
        try:
            stdout = sys.stdout
            sys.stdout = open(os.devnull, "w")
            try:
                self.u.main()
            finally:
                sys.stdout.close()
                sys.stdout = stdout
        finally:
            sys.argv = argv

    def calls(self, name="rauc"):
        try:
            with open(self.log) as f:
                return [line.strip() for line in f if line.startswith(name)]
        except OSError:
            return []

    def check(self):
        with open(self.u.CHECK) as f:
            return json.load(f)

    def history(self):
        with open(self.u.HISTORY) as f:
            return json.load(f)

    def tried(self):
        try:
            with open(self.u.TRIED) as f:
                return f.read().strip()
        except OSError:
            return ""


class Timer(Base):
    def test_up_to_date(self):
        self.offer("v1")
        self.write("rauc/update-tried", "v0\n")
        self.run_it()
        self.assertEqual(self.check()["detail"], "up to date")
        self.assertTrue(self.check()["last_success"])
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.tried(), "")

    def test_installs_and_waits_for_a_reboot(self):
        self.offer()
        self.run_it()
        self.assertEqual(self.calls(), [self.installed])
        self.assertEqual(self.tried(), "v2")
        self.assertEqual(self.history()[0]["state"], "ok")
        self.assertEqual(self.history()[0]["source"], "automatic")
        self.assertEqual(self.check()["detail"], "installed v2")
        self.assertEqual(self.calls("systemctl reboot"), [])

    def test_reboots_when_the_owner_said_to(self):
        self.offer()
        self.write("provision/update-reboot", "")
        self.run_it()
        self.assertEqual(self.calls("systemctl reboot"), ["systemctl reboot"])

    def test_holds_for_a_person(self):
        self.offer()
        self.write("provision/update-hold", "")
        self.write("provision/update-reboot", "")
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.check()["offered"], "v2")
        self.assertEqual(self.calls("systemctl reboot"), [])

    def test_leaves_a_unit_thats_about_to_restart_alone(self):
        self.offer()
        self.write("rebooting", "")
        self.run_it()
        self.assertEqual(self.calls(), [])

    def test_a_failed_install_is_tried_again(self):
        self.offer()
        self.write("fail", "")
        self.run_it()
        self.assertFalse(self.check()["ok"])
        self.assertEqual(self.tried(), "")
        self.assertEqual(self.history()[0]["state"], "failed")
        self.assertEqual(self.history()[0]["detail"], "the state on this unit can't be read")
        os.remove(self.fail_flag)
        self.run_it()
        self.assertEqual(len(self.calls()), 2)
        self.assertEqual(self.tried(), "v2")

    def test_a_release_that_rolled_back_doesnt_loop(self):
        self.offer()
        self.write("rauc/update-tried", "v2\n")
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertIn("was installed", self.check()["detail"])
        self.assertEqual(self.check()["offered"], "v2")

    def test_a_full_address_stays_as_it_is(self):
        elsewhere = f"localhost:{self.server.server_address[1]}/v2/x.raucb"
        self.offer(bundle=f"http://{elsewhere}")
        self.run_it()
        self.assertIn(elsewhere, self.server.requests)
        self.assertEqual(self.calls(), [self.installed])

    def test_a_channel_that_sends_it_elsewhere(self):
        self.write("provision/update-channel", f"{self.channel}/moved\n")
        self.offer(bundle=f"{self.channel}/moved/v2/x.raucb")
        self.run_it()
        here = f"localhost:{self.server.server_address[1]}"
        self.assertIn(f"{here}/latest.json", self.server.requests)
        self.assertIn(f"{here}/v2/x.raucb", self.server.requests)
        self.assertEqual(self.calls(), [self.installed])

    def test_a_dead_channel_is_a_result(self):
        self.write("provision/update-channel", "http://127.0.0.1:9\n")
        self.run_it()
        self.assertFalse(self.check()["ok"])
        self.assertIn("can't reach", self.check()["detail"])
        self.assertNotIn("curl", self.check()["detail"])

    def test_a_failure_keeps_the_last_success(self):
        self.offer("v1")
        self.run_it()
        success = self.check()["last_success"]
        os.remove(os.path.join(self.www, "latest.json"))
        self.run_it()
        self.assertFalse(self.check()["ok"])
        self.assertEqual(self.check()["last_success"], success)

    def test_what_a_server_sends_isnt_trusted(self):
        # An unsigned file is one of these.
        for body in ("not json", "[]", "null", '{"version": "v2", "bundle": "v2/x.raucb"}', "x" * (70 * 1024)):
            self.write("www/latest.json", body)
            self.run_it()
            self.assertFalse(self.check()["ok"], body)
            self.assertEqual(self.check()["offered"], None, body)
            self.assertEqual(self.calls(), [], body)
        self.write("www/latest.json", '{"version": "v2", "bundle": "v2/x.raucb"}')
        self.run_it()
        self.assertIn("isn't signed by the fleet: its signature doesn't match", self.check()["detail"])

    def test_even_whats_signed_is_checked(self):
        for changes in ({"version": 2}, {"version": "a b"}, {"version": "-rf"}, {"bundle": "file:///etc/shadow"},
                        {"bundle": "--help://x"}, {"bundle": "http://a/b c"}, {"bundle": "a\nb"}, {"bundle": None},
                        {"manifest_hash": "AB" * 32}, {"manifest_hash": "--x"}, {"sequence": "1"}, {"sequence": True},
                        {"sequence": 1.5}):
            self.offer(**changes)
            self.run_it()
            self.assertFalse(self.check()["ok"], changes)
            self.assertEqual(self.check()["offered"], None, changes)
            self.assertEqual(self.calls(), [], changes)


class Person(Base):
    def test_check_changes_nothing(self):
        self.offer()
        self.run_it("check")
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.check()["offered"], "v2")

    def test_no_channel(self):
        os.remove(self.u.CHANNEL)
        self.run_it("check")
        self.assertEqual(self.check()["detail"], "no update channel is set")

    def test_install_never_reboots(self):
        self.offer()
        self.write("provision/update-reboot", "")
        self.run_it("install", "v2", "7")
        self.assertEqual(self.history()[0]["source"], "channel")
        self.assertEqual(self.history()[0]["job"], 7)
        self.assertEqual(self.calls("systemctl reboot"), [])

    def test_can_try_a_release_again(self):
        self.offer()
        self.write("rauc/update-tried", "v2\n")
        self.run_it("install", "v2", "7")
        self.assertEqual(len(self.calls()), 1)

    def test_gets_what_they_were_shown_or_nothing(self):
        self.offer("v3")
        self.run_it("install", "v2", "7")
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.history()[0]["state"], "failed")
        self.assertEqual(self.history()[0]["job"], 7)
        self.assertIn("v3", self.history()[0]["detail"])

    def test_a_file(self):
        self.run_it("upload", "v9", "7")
        self.assertEqual(self.calls(), [f"rauc install {self.u.BUNDLE}"])
        self.assertEqual(self.tried(), "v9")
        self.assertEqual(self.history()[0]["source"], "file")
        self.assertFalse(os.path.exists(self.u.BUNDLE))
        self.assertFalse(os.path.exists(self.u.BUNDLE_INFO))

    def test_a_refused_file_doesnt_stay_in_memory(self):
        self.write("fail", "")
        self.run_it("upload", "v9", "7")
        self.assertEqual(self.history()[0]["state"], "failed")
        self.assertFalse(os.path.exists(self.u.BUNDLE))

    def test_an_interrupted_install_says_so_next_time(self):
        self.offer()
        self.write("report/update-history.json", json.dumps([{"version": "v2", "state": "running", "detail": ""}]))
        self.run_it("install", "v2", "7")
        self.assertEqual(self.history()[1]["state"], "failed")
        self.assertEqual(self.history()[1]["detail"], "it was interrupted")

    def test_history_stays_small(self):
        self.write("report/update-history.json", json.dumps([{"version": "old", "state": "ok"}] * 40))
        self.offer()
        self.run_it("install", "v2", "7")
        self.assertEqual(len(self.history()), self.u.KEEP)

    def test_one_at_a_time(self):
        import fcntl
        self.offer()
        with open(self.u.LOCK, "w") as held:
            fcntl.flock(held, fcntl.LOCK_EX)
            self.run_it("install", "v2", "7")
        self.assertEqual(self.calls(), [])

    def test_a_damaged_results_file_doesnt_stop_it(self):
        self.offer()
        self.write("report/update-history.json", "{not json")
        self.write("report/update-check.json", "[]")
        self.run_it("install", "v2", "7")
        self.assertEqual(self.history()[0]["state"], "ok")

    def test_gets_only_the_bundle_the_file_named(self):
        self.offer()
        self.run_it("install", "v2", "7")
        self.assertEqual(self.calls(), [self.installed])


class Signed(Base):
    """What stops a file server offering an older release (docs/updates.md, Channels)."""

    def refused(self, *args, says):
        self.run_it(*args)
        self.assertFalse(self.check()["ok"])
        self.assertIn(says, self.check()["detail"])
        self.assertEqual(self.check()["offered"], None)
        self.assertEqual(self.calls(), [])

    def test_the_console_sees_the_channel_and_when_it_was_signed(self):
        self.offer()
        self.run_it("check")
        self.assertEqual(self.check()["channel"], "stable")
        self.assertEqual(self.check()["sequence"], self.sequence)

    def test_a_unit_has_to_know_its_channels_name(self):
        os.remove(self.u.CHANNEL_NAME)
        self.offer()
        self.refused(says="hasn't been told the name of its update channel")

    def test_another_channels_file(self):
        self.offer(channel="beta")
        self.refused(says="for the channel beta, and this unit follows stable")

    def test_another_products_file(self):
        self.offer(compatible="something-else")
        self.refused(says="is for something-else, not this product")

    def test_an_older_file_than_one_seen(self):
        self.offer("v3")
        self.run_it("check")
        newest = self.sequence
        self.offer("v2", sequence=newest - 60)
        self.refused(says="this unit has seen a newer one from it")
        self.assertEqual(self.floors()["stable"]["sequence"], newest)
        # And a person can't install it from the channel either.
        self.run_it("install", "v2", "7")
        self.assertEqual(self.history()[0]["state"], "failed")
        self.assertEqual(self.calls(), [])

    def test_a_newer_file_naming_an_older_release_rolls_back_on_purpose(self):
        self.write("release", "v3\n")
        self.offer("v3")
        self.run_it("check")
        self.offer("v2")
        self.run_it()
        self.assertEqual(self.calls(), [self.installed])

    def test_the_same_file_again(self):
        self.offer(sequence=self.sequence)
        self.run_it("check")
        self.run_it("check")
        self.assertTrue(self.check()["ok"])

    def test_a_different_file_signed_at_the_same_time(self):
        self.offer("v2", sequence=self.sequence)
        self.run_it("check")
        self.offer("v3", sequence=self.sequence)
        self.refused(says="isn't the one this unit saw signed at the same time")

    def test_the_floor_moves_when_its_seen_not_when_its_installed(self):
        self.write("provision/update-hold", "")
        self.offer()
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.floors()["stable"]["sequence"], self.sequence)

    def test_each_channel_has_its_own_floor(self):
        self.offer(sequence=2000)
        self.run_it("check")
        self.write("provision/update-channel-name", "beta\n")
        self.offer(channel="beta", sequence=1000)
        self.run_it("check")
        self.assertTrue(self.check()["ok"])
        self.write("provision/update-channel-name", "stable\n")
        self.offer(sequence=1500)
        self.refused(says="this unit has seen a newer one from it")
        self.assertEqual(self.floors(), {"stable": {"sequence": 2000, "payload": self.floors()["stable"]["payload"]},
                                         "beta": {"sequence": 1000, "payload": self.floors()["beta"]["payload"]}})

    def test_nothing_signed_before_this_release_was_built(self):
        self.write("built-at", f"{self.sequence + 100}\n")
        self.offer(sequence=self.sequence)
        self.refused(says="before this release was built")
        self.assertFalse(os.path.exists(self.u.FLOORS))
        self.offer(sequence=self.sequence + 100)
        self.run_it("check")
        self.assertTrue(self.check()["ok"])

    def test_a_floor_that_cant_be_read_stops_the_channel(self):
        for body in ("{not json", "[]", '{"stable": {"sequence": "2000", "payload": "x"}}', '{"stable": 2000}'):
            self.write("rauc/channel-floors.json", body)
            self.offer()
            self.refused(says="can't be read, and it says how new a file this unit has seen")
            with open(self.u.FLOORS) as f:
                self.assertEqual(f.read(), body)

    def test_a_file_the_fleet_didnt_sign(self):
        self.write("www/latest.json", json.dumps({"payload": "x", "signature": "y"}))
        self.refused(says="isn't signed by the fleet")

    def test_no_checker_installs_nothing(self):
        os.remove(self.u.VERIFY)
        self.offer()
        self.refused(says="couldn't be checked")

    def racing(self, meanwhile):
        """Runs meanwhile just after a check takes a file in, the way a person's check or an owner saving settings
        can, since neither waits for the timer. meanwhile gets the real accept."""
        accept = self.u.accept

        def racing_accept(*args):
            accept(*args)
            meanwhile(accept)
        self.u.accept = racing_accept
        self.addCleanup(setattr, self.u, "accept", accept)

    def test_a_newer_file_seen_meanwhile_stops_the_timer(self):
        self.offer()
        self.racing(lambda accept: accept("stable", self.sequence + 100, "a newer one"))
        self.run_it()
        self.assertEqual(self.calls(), [])

    def test_a_newer_file_seen_meanwhile_stops_a_person(self):
        self.offer()
        self.racing(lambda accept: accept("stable", self.sequence + 100, "a newer one"))
        self.run_it("install", "v2", "7")
        self.assertEqual(self.calls(), [])
        self.assertIn("changed while it was being checked", self.history()[0]["detail"])

    def test_a_channel_changed_meanwhile_installs_nothing_and_says_nothing(self):
        original = self.u.accept
        for change in (lambda accept: self.write("provision/update-channel-name", "beta\n"),
                       lambda accept: os.remove(self.u.CHANNEL),
                       lambda accept: self.write("provision/update-channel", "http://127.0.0.1:9\n")):
            with self.subTest():
                self.write("provision/update-channel", self.channel + "\n")
                self.write("provision/update-channel-name", "stable\n")
                if os.path.exists(self.u.CHECK):
                    os.remove(self.u.CHECK)
                self.offer()
                self.racing(change)
                self.run_it()
                self.u.accept = original
                self.assertEqual(self.calls(), [])
                # The console forgot the old channel's answer when the settings changed, and it stays forgotten.
                self.assertFalse(os.path.exists(self.u.CHECK))

    def test_a_file_is_still_the_owners_call(self):
        self.offer("v3")
        self.run_it("check")
        self.run_it("upload", "v1", "7")
        self.assertEqual(self.calls(), [f"rauc install {self.u.BUNDLE}"])


class Download(Base):
    """A bundle from a channel is downloaded whole before RAUC sees it (docs/updates.md, Streaming from object
    storage)."""

    def detail(self):
        return self.history()[0]["detail"]

    def test_whole_then_installed_then_gone(self):
        self.offer()
        self.run_it()
        self.assertEqual(self.calls(), [self.installed])
        self.assertEqual(self.calls("bundle"), [f"bundle {len(BUNDLE)}"])
        self.assertFalse(os.path.exists(self.downloaded))
        self.assertFalse(os.path.exists(self.u.DOWNLOAD))
        self.assertEqual(os.stat(self.u.IN_MEMORY).st_mode & 0o777, 0o700)

    def test_the_console_is_told_what_is_downloading(self):
        self.offer()
        self.run_it("install", "v2", "7")
        said = json.loads(self.calls("download")[0].split(" ", 1)[1])
        self.assertEqual(said, {"version": "v2", "file": self.downloaded, "total": len(BUNDLE)})

    def test_on_the_data_disk_when_there_is_one(self):
        ismount = self.u.os.path.ismount
        self.u.os.path.ismount = lambda path: path == self.u.DATA or ismount(path)
        self.addCleanup(setattr, self.u.os.path, "ismount", ismount)
        self.offer()
        self.run_it()
        on_disk = os.path.join(self.u.ON_DISK, "update.raucb")
        self.assertEqual(self.calls(), [f"rauc install --require-manifest-hash={HASH} {on_disk}"])
        self.assertFalse(os.path.exists(on_disk))

    def test_no_room(self):
        self.write("meminfo", "MemAvailable: 1000 kB\n")
        self.offer(size=3 << 30)
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertIn("hasn't room to download the release", self.detail())
        self.assertFalse(os.path.exists(self.downloaded))
        self.assertEqual(self.tried(), "")

    def test_a_server_that_sends_more_is_cut_off(self):
        self.offer(size=100)
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.detail(), "the update channel sent more than its file said the release is")
        self.assertFalse(os.path.exists(self.downloaded))

    def test_a_server_that_sends_less(self):
        self.offer(size=len(BUNDLE) + 1)
        self.run_it("install", "v2", "7")
        self.assertEqual(self.calls(), [])
        self.assertIn(f"sent {len(BUNDLE)} bytes of v2", self.detail())
        self.assertFalse(os.path.exists(self.downloaded))

    def test_a_bundle_that_isnt_there(self):
        self.offer(bundle="v2/missing.raucb")
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertIn("downloading v2 failed: ", self.detail())
        self.assertIn("404", self.detail())
        self.assertFalse(self.check()["ok"])

    def test_a_file_without_a_size_is_refused(self):
        self.offer(size=None)
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertFalse(self.check()["ok"])

    def test_a_stalled_download_is_given_up(self):
        self.offer(bundle="v2/stalls.raucb")
        started = time.monotonic()
        self.run_it()
        self.assertLess(time.monotonic() - started, 30)
        self.assertEqual(self.calls(), [])
        self.assertIn("downloading v2 failed: ", self.detail())
        self.assertFalse(os.path.exists(self.downloaded))

    def test_a_failed_try_is_tried_again(self):
        self.offer(bundle="v2/flaky.raucb")
        self.run_it()
        self.assertEqual(self.calls(), [self.installed])
        self.assertEqual(self.calls("bundle"), [f"bundle {len(BUNDLE)}"])

    def test_a_server_cant_make_it_wait(self):
        self.offer(bundle="v2/busy.raucb")
        started = time.monotonic()
        self.run_it()
        self.assertLess(time.monotonic() - started, 10)
        self.assertEqual(self.calls(), [])
        self.assertIn("503", self.detail())
        self.assertEqual(sum(r.endswith("/busy.raucb") for r in self.server.requests), self.u.TRIES)

    def test_a_download_has_a_deadline(self):
        self.u.MIN_TIME_S, self.u.MIN_RATE = 1, 1 << 40
        self.offer(bundle="v2/stalls.raucb")
        started = time.monotonic()
        self.run_it()
        self.assertLess(time.monotonic() - started, 5)
        self.assertEqual(self.calls(), [])
        self.assertIn("downloading v2 took too long", self.detail())
        self.assertFalse(os.path.exists(self.downloaded))

    def test_a_directory_others_can_reach_isnt_used(self):
        os.makedirs(self.u.IN_MEMORY, mode=0o755)
        os.chmod(self.u.IN_MEMORY, 0o755)
        self.write("run/patai-update/update.raucb", "someone else's")
        self.offer()
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertIn("isn't private", self.detail())
        self.assertTrue(os.path.exists(self.downloaded))

    def test_what_a_power_cut_left_is_cleared_first(self):
        for directory in (self.u.ON_DISK, self.u.IN_MEMORY):
            os.makedirs(directory, mode=0o700)
            with open(os.path.join(directory, "update.raucb"), "wb") as f:
                f.write(b"half a bundle")
        self.offer()
        self.run_it()
        self.assertEqual(self.calls(), [self.installed])
        self.assertEqual(self.calls("bundle"), [f"bundle {len(BUNDLE)}"])
        self.assertFalse(os.path.exists(os.path.join(self.u.ON_DISK, "update.raucb")))

    def meanwhile(self, change):
        download = self.u.download

        def downloading(*args):
            path = download(*args)
            change()
            return path
        self.u.download = downloading
        self.addCleanup(setattr, self.u, "download", download)

    def test_a_channel_changed_while_downloading(self):
        self.offer()
        self.meanwhile(lambda: self.write("provision/update-channel-name", "beta\n"))
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.detail(), "the channel changed while the release was downloading, so it wasn't installed")
        self.assertFalse(os.path.exists(self.downloaded))

    def test_a_newer_file_seen_while_downloading(self):
        self.offer()
        self.meanwhile(lambda: self.u.accept("stable", self.sequence + 100, "a newer one"))
        self.run_it("install", "v2", "7")
        self.assertEqual(self.calls(), [])
        self.assertIn("changed while the release was downloading", self.detail())

    def test_the_owner_chose_to_wait_while_downloading(self):
        self.offer()
        self.meanwhile(lambda: self.write("provision/update-hold", ""))
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertIn("wait for a person while it was downloading", self.detail())
        # It doesn't bind a person.
        self.offer()
        self.run_it("install", "v2", "7")
        self.assertEqual(self.calls(), [self.installed])

    def test_a_restart_asked_for_while_downloading(self):
        self.offer()
        self.meanwhile(lambda: self.write("rebooting", ""))
        self.run_it()
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.detail(), "the unit is restarting, so the release wasn't installed")
        self.assertEqual(self.tried(), "")


if __name__ == "__main__":
    unittest.main(argv=["patai-update"], verbosity=1)
