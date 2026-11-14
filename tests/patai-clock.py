#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-clock, with stand-ins for hwclock, timedatectl and the RTCs in
sysfs. It has to pass on a unit's Python 3.8:

    python3 tests/patai-clock.py
    scripts/ssh python3 - < tests/patai-clock.py    (with the script in /usr/local/sbin)
"""
import calendar
import importlib.machinery
import importlib.util
import json
import os
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__)) if "__file__" in globals() else "/nonexistent"
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-clock")
if not os.path.exists(SCRIPT):
    SCRIPT = "/usr/local/sbin/patai-clock"

Y2000 = calendar.timegm((2000, 1, 1, 1, 0, 21))
RELEASE = calendar.timegm((2026, 11, 2, 12, 0, 0))
HALTED = calendar.timegm((2026, 11, 2, 20, 1, 29))
LATER = calendar.timegm((2026, 11, 3, 9, 30, 0))

STUB = r"""#!/usr/bin/env python3
import os, sys
with open(os.environ["STUB_LOG"], "a") as f:
    f.write(os.path.basename(sys.argv[0]) + " " + " ".join(sys.argv[1:]) + "\n")
args = sys.argv[1:]
"""
HWCLOCK = STUB + r"""
if os.path.exists(os.environ["STUB_HWCLOCK_FAILS"]):
    print("hwclock: ioctl(RTC_SET_TIME) to /dev/rtc1 to set the time failed: Invalid argument", file=sys.stderr)
    sys.exit(1)
"""
TIMEDATECTL = STUB + r"""
if args == ["show", "-p", "NTPSynchronized", "--value"]:
    print("yes" if os.path.exists(os.environ["STUB_SYNCED"]) else "no")
elif args == ["show", "-p", "NTP", "--value"]:
    print("yes")
elif args[:1] == ["show-timesync"]:
    print("1.pool.ntp.org" if os.path.exists(os.environ["STUB_SYNCED"]) else "")
"""


def load_script():
    loader = importlib.machinery.SourceFileLoader("patai_clock", SCRIPT)
    spec = importlib.util.spec_from_loader("patai_clock", loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = self.root = self.tmp.name
        for name in ("bin", "run", "rtc/rtc0", "rtc/rtc1"):
            os.makedirs(os.path.join(root, name))
        self.write("rtc/rtc0/name", "tegra_rtc c2a0000.rtc\n")
        self.write("rtc/rtc1/name", "max77686-rtc max77620-rtc\n")
        self.write("rtc/rtc0/since_epoch", f"{Y2000}\n")
        self.write("patai-version", "2026.11.02-1\n")
        os.utime(f"{root}/patai-version", (RELEASE, RELEASE))
        self.write("timesync-clock", "")
        os.utime(f"{root}/timesync-clock", (HALTED, HALTED))
        self.log = os.path.join(root, "calls")
        os.environ.update(STUB_LOG=self.log, STUB_SYNCED=f"{root}/synced", STUB_HWCLOCK_FAILS=f"{root}/hwclock-fails")
        for name, text in (("hwclock", HWCLOCK), ("timedatectl", TIMEDATECTL), ("systemctl", STUB)):
            self.write(f"bin/{name}", text)
            os.chmod(f"{root}/bin/{name}", 0o755)
        self.path = os.environ["PATH"]
        os.environ["PATH"] = f"{root}/bin:{self.path}"

        self.clock = load_script()
        self.clock.VERSION = f"{root}/patai-version"
        self.clock.TIMESYNC = f"{root}/timesync-clock"
        self.clock.SYS_RTC = f"{root}/rtc"
        self.clock.BOOT = f"{root}/run/clock-boot.json"
        self.clock.SET = f"{root}/run/clock-set.json"
        self.clock.LOCK = f"{root}/run/clock.lock"
        self.time = float(HALTED)
        self.uptime = 30.0
        self.settings = []
        self.clock.now = lambda: self.time
        self.clock.boottime = lambda: self.uptime

        def set_clock(seconds):
            self.settings.append(seconds)
            self.time = seconds
        self.clock.set_clock = set_clock

    def tearDown(self):
        os.environ["PATH"] = self.path
        self.tmp.cleanup()

    def write(self, name, text):
        with open(os.path.join(self.root, name), "w") as f:
            f.write(text)

    def rtc(self, seconds):
        self.write("rtc/rtc1/since_epoch", f"{seconds}\n")

    def calls(self):
        try:
            with open(self.log) as f:
                return f.read().splitlines()
        except FileNotFoundError:
            return []

    def synced(self):
        self.write("synced", "")


class Boot(Base):
    def test_an_rtc_that_kept_its_time_sets_the_clock_as_nvidias_script_did(self):
        self.rtc(LATER)
        self.clock.boot()
        self.assertEqual(self.calls(), ["hwclock --hctosys --utc --noadjfile -f /dev/rtc1",
                                        "hwclock --systohc --utc --noadjfile -f /dev/rtc0"])
        self.assertFalse(self.clock.status()["rtc_lost"])

    def test_an_rtc_back_at_2000_is_ignored_and_set_from_the_clock(self):
        self.rtc(Y2000)
        self.clock.boot()
        self.assertNotIn("hwclock --hctosys --utc --noadjfile -f /dev/rtc1", self.calls())
        self.assertIn("hwclock --systohc --utc --noadjfile -f /dev/rtc1", self.calls())
        self.assertEqual(self.settings, [])
        self.assertTrue(self.clock.status()["rtc_lost"])

    def test_a_clock_still_behind_what_timesyncd_saved_goes_up_to_it(self):
        self.rtc(Y2000)
        self.time = float(Y2000)
        self.clock.boot()
        self.assertEqual(self.settings, [HALTED])

    def test_without_what_timesyncd_saved_the_release_is_the_floor(self):
        os.remove(f"{self.root}/timesync-clock")
        self.rtc(Y2000)
        self.time = float(Y2000)
        self.clock.boot()
        self.assertEqual(self.settings, [RELEASE])

    def test_an_rtc_past_2055_is_no_better_than_one_at_2000(self):
        future = calendar.timegm((2099, 1, 1, 0, 0, 0))
        self.rtc(future)
        self.time = float(future)  # the kernel took it before this ran
        self.clock.boot()
        self.assertNotIn("hwclock --hctosys --utc --noadjfile -f /dev/rtc1", self.calls())
        self.assertEqual(self.settings, [HALTED])
        self.assertTrue(self.clock.status()["rtc_lost"])

    def test_a_saved_time_past_2055_isnt_a_floor_either(self):
        future = calendar.timegm((2099, 1, 1, 0, 0, 0))
        self.rtc(future)
        os.utime(f"{self.root}/timesync-clock", (future, future))
        self.time = float(future)
        self.clock.boot()
        self.assertEqual(self.settings, [RELEASE])

    def test_an_rtc_that_cant_be_read_counts_as_lost(self):
        self.assertFalse(os.path.exists(f"{self.root}/rtc/rtc1/since_epoch"))
        self.clock.boot()
        self.assertTrue(self.clock.status()["rtc_lost"])

    def test_no_backup_rtc_leaves_the_clock_alone(self):
        os.rename(f"{self.root}/rtc/rtc1", f"{self.root}/elsewhere")
        self.clock.boot()
        self.assertEqual(self.calls(), ["hwclock --systohc --utc --noadjfile -f /dev/rtc0"])
        self.assertFalse(self.clock.status()["rtc_lost"])


class Set(Base):
    def test_it_sets_the_clock_and_the_backup_rtc_and_says_so(self):
        self.time = float(Y2000)
        result = self.clock.set_time(str(LATER * 1000))
        self.assertEqual(self.settings, [LATER])
        self.assertIn("hwclock --systohc --utc --noadjfile -f /dev/rtc1", self.calls())
        self.assertIn("systemctl start --no-block patai-report.service", self.calls())
        self.assertTrue(result["rtc_written"])
        self.assertEqual(result["set"], {"from_ms": Y2000 * 1000, "to_ms": LATER * 1000, "ago_s": 0})
        self.assertEqual(result["now_ms"], LATER * 1000)

    def test_it_moves_timesyncds_saved_time_either_way(self):
        self.assertTrue(self.clock.set_time(str(LATER * 1000))["floor_saved"])
        self.assertEqual(os.stat(f"{self.root}/timesync-clock").st_mtime, LATER)
        self.clock.set_time(str((RELEASE + 60) * 1000))
        self.assertEqual(os.stat(f"{self.root}/timesync-clock").st_mtime, RELEASE + 60)

    def test_a_unit_that_never_synced_gets_a_saved_time_too(self):
        os.remove(f"{self.root}/timesync-clock")
        self.assertTrue(self.clock.set_time(str(LATER * 1000))["floor_saved"])
        self.assertEqual(os.stat(f"{self.root}/timesync-clock").st_mtime, LATER)

    def test_a_saved_time_that_cant_be_kept_says_so(self):
        self.clock.TIMESYNC = f"{self.root}/no-such-dir/clock"
        result = self.clock.set_time(str(LATER * 1000))
        self.assertEqual(self.settings, [LATER])
        self.assertFalse(result["floor_saved"])

    def test_the_record_says_how_long_ago(self):
        self.clock.set_time(str(LATER * 1000))
        self.uptime += 125
        self.assertEqual(self.clock.status()["set"]["ago_s"], 125)

    def test_ntp_keeps_the_clock(self):
        self.synced()
        with self.assertRaisesRegex(self.clock.Refused, "NTP keeps"):
            self.clock.set_time(str(LATER * 1000))
        self.assertEqual(self.settings, [])

    def test_a_time_before_the_release_cant_be_right(self):
        with self.assertRaisesRegex(self.clock.Refused, "before this release"):
            self.clock.set_time(str((RELEASE - 1) * 1000))
        self.assertEqual(self.settings, [])

    def test_a_time_after_the_certificate_cant_be_right(self):
        with self.assertRaisesRegex(self.clock.Refused, "certificate"):
            self.clock.set_time(str(self.clock.LATEST * 1000))

    def test_only_a_number_is_a_time(self):
        for text in ("", "-1", "1e12", "17936366350001234", "1793636635000; reboot"):
            with self.assertRaisesRegex(self.clock.Refused, "isn't a time"):
                self.clock.set_time(text)
        self.assertEqual(self.settings, [])

    def test_an_rtc_that_wont_take_it_still_leaves_the_clock_set(self):
        self.write("hwclock-fails", "")
        result = self.clock.set_time(str(LATER * 1000))
        self.assertEqual(self.settings, [LATER])
        self.assertFalse(result["rtc_written"])


class Status(Base):
    def test_status_says_what_keeps_the_clock(self):
        self.synced()
        status = self.clock.status()
        self.assertEqual(status["now_ms"], HALTED * 1000)
        self.assertTrue(status["synced"])
        self.assertEqual(status["server"], "1.pool.ntp.org")
        self.assertIsNone(status["set"])
        self.assertEqual(status["earliest_ms"], RELEASE * 1000)
        self.assertEqual(status["latest_ms"], self.clock.LATEST * 1000)
        json.dumps(status)

    def test_status_before_boot_has_run(self):
        status = self.clock.status()
        self.assertFalse(status["synced"])
        self.assertIsNone(status["server"])
        self.assertFalse(status["rtc_lost"])


if __name__ == "__main__":
    unittest.main()
