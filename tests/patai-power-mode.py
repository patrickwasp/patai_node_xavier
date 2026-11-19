#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-power-mode, with a stand-in nvpmodel. It has to
pass on a unit's Python 3.8:

    python3 tests/patai-power-mode.py
    scripts/ssh python3 - < tests/patai-power-mode.py    (with the script in /usr/local/sbin)
"""
import importlib.machinery
import importlib.util
import os
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__)) if "__file__" in globals() else "/nonexistent"
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-power-mode")
if not os.path.exists(SCRIPT):
    SCRIPT = "/usr/local/sbin/patai-power-mode"

# The shape of /etc/nvpmodel/nvpmodel_t194.conf, cut down.
CONF = """# < POWER_MODEL ID=id_num NAME=mode_name >
< PARAM TYPE=CLOCK NAME=GPU >
MAX_FREQ /sys/devices/gpu.0/devfreq/17000000.gv11b/max_freq
< POWER_MODEL ID=0 NAME=MAXN >
CPU_ONLINE CORE_0 1
CPU_ONLINE CORE_1 1
CPU_ONLINE CORE_2 1
CPU_ONLINE CORE_3 1
CPU_ONLINE CORE_4 1
CPU_ONLINE CORE_5 1
CPU_ONLINE CORE_6 1
CPU_ONLINE CORE_7 1
CPU_DENVER_0 MIN_FREQ 1200000
CPU_DENVER_0 MAX_FREQ -1
GPU MIN_FREQ 0
GPU MAX_FREQ -1
< POWER_MODEL ID=1 NAME=MODE_10W >
CPU_ONLINE CORE_0 1
CPU_ONLINE CORE_1 1
CPU_ONLINE CORE_2 0
CPU_ONLINE CORE_3 0
CPU_DENVER_0 MAX_FREQ 1200000
GPU MAX_FREQ 520200000
< POWER_MODEL ID=7 NAME=MODE_15W_DESKTOP >
CPU_ONLINE CORE_0 1
CPU_ONLINE CORE_1 1
CPU_ONLINE CORE_2 1
CPU_ONLINE CORE_3 1
CPU_ONLINE CORE_4 0
CPU_DENVER_0 MAX_FREQ 2188800
GPU MAX_FREQ 670000000
< PM_CONFIG DEFAULT=7 >
"""

NVPMODEL = r"""#!/usr/bin/env python3
import os, sys
with open(os.environ["STUB_LOG"], "a") as f:
    f.write("nvpmodel " + " ".join(sys.argv[1:]) + "\n")
state = os.environ["STUB_MODE"]
names = {"0": "MAXN", "1": "MODE_10W", "7": "MODE_15W_DESKTOP"}
args = sys.argv[1:]
if args == ["-q"]:
    mode = open(state).read().strip()
    print(f"NV Power Mode: {names[mode]}\n{mode}")
    print("NVPM ERROR: Error opening /sys/kernel/nvpmodel_emc_cap/emc_iso_cap: 13", file=sys.stderr)
elif args[:1] == ["-m"]:
    if os.path.exists(os.environ["STUB_FAILS"]):
        print("NVPM ERROR: failed to set power mode", file=sys.stderr)
        sys.exit(1)
    if not os.path.exists(os.environ["STUB_IGNORES"]):
        open(state, "w").write(args[1])
"""


def load_script():
    loader = importlib.machinery.SourceFileLoader("patai_power_mode", SCRIPT)
    spec = importlib.util.spec_from_loader("patai_power_mode", loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = self.root = self.tmp.name
        for name in ("bin", "persist", "run"):
            os.makedirs(os.path.join(root, name))
        self.write("nvpmodel.conf", CONF)
        self.write("mode", "7\n")
        self.log = os.path.join(root, "calls")
        os.environ.update(STUB_LOG=self.log, STUB_MODE=f"{root}/mode", STUB_FAILS=f"{root}/fails",
                          STUB_IGNORES=f"{root}/ignores")
        self.write("bin/nvpmodel", NVPMODEL)
        os.chmod(f"{root}/bin/nvpmodel", 0o755)
        self.path = os.environ["PATH"]
        os.environ["PATH"] = f"{root}/bin:{self.path}"

        self.pm = load_script()
        self.pm.CONF = f"{root}/nvpmodel.conf"
        self.pm.SAVED = f"{root}/persist/power-mode"
        self.pm.LOCK = f"{root}/run/power-mode.lock"

    def tearDown(self):
        os.environ["PATH"] = self.path
        self.tmp.cleanup()

    def write(self, name, text):
        with open(os.path.join(self.root, name), "w") as f:
            f.write(text)

    def read(self, name):
        with open(os.path.join(self.root, name)) as f:
            return f.read().strip()

    def calls(self):
        try:
            with open(self.log) as f:
                return f.read().splitlines()
        except FileNotFoundError:
            return []


class Modes(Base):
    def test_the_modes_come_from_the_release_conf(self):
        found, default = self.pm.modes()
        # MAXN, not nvpmodel's default.
        self.assertEqual(default, 0)
        self.assertEqual(found, [
            {"id": 0, "name": "MAXN", "cores": 8, "cpu_mhz": None, "gpu_mhz": None},
            {"id": 1, "name": "MODE_10W", "cores": 2, "cpu_mhz": 1200, "gpu_mhz": 520},
            {"id": 7, "name": "MODE_15W_DESKTOP", "cores": 4, "cpu_mhz": 2188, "gpu_mhz": 670},
        ])

    def test_status(self):
        self.write("persist/power-mode", "0\n")
        status = self.pm.status()
        self.assertEqual((status["current"], status["saved"], status["default"]), (7, 0, 0))
        self.assertEqual(len(status["modes"]), 3)


class Change(Base):
    def test_a_change_happens_now_and_is_kept(self):
        self.assertEqual(self.pm.change("0")["name"], "MAXN")
        self.assertEqual(self.read("mode"), "0")
        self.assertEqual(self.read("persist/power-mode"), "0")
        self.assertEqual(self.pm.status()["saved"], 0)

    def test_only_a_mode_this_release_has(self):
        for text in ("3", "", "-1", "0 1", "7; reboot"):
            with self.assertRaisesRegex(self.pm.Refused, "no power mode"):
                self.pm.change(text)
        self.assertEqual(self.calls(), [])

    def test_a_mode_nvpmodel_refuses_isnt_kept(self):
        self.write("fails", "")
        with self.assertRaisesRegex(self.pm.Refused, "nvpmodel failed"):
            self.pm.change("0")
        self.assertFalse(os.path.exists(f"{self.root}/persist/power-mode"))

    def test_a_mode_that_didnt_take_isnt_kept(self):
        self.write("ignores", "")
        with self.assertRaisesRegex(self.pm.Refused, "didn't change"):
            self.pm.change("0")
        self.assertFalse(os.path.exists(f"{self.root}/persist/power-mode"))


class Boot(Base):
    def test_the_kept_mode_comes_back(self):
        self.write("persist/power-mode", "0\n")
        self.pm.boot()
        self.assertEqual(self.read("mode"), "0")

    def test_nothing_kept_is_maxn(self):
        self.pm.boot()
        self.assertEqual(self.read("mode"), "0")

    def test_without_maxn_its_nvpmodels_default(self):
        self.write("nvpmodel.conf", CONF.replace("NAME=MAXN", "NAME=MODE_30W"))
        self.write("mode", "0\n")
        self.pm.boot()
        self.assertEqual(self.read("mode"), "7")

    def test_a_kept_mode_this_release_doesnt_have_is_the_default(self):
        self.write("mode", "7\n")
        self.write("persist/power-mode", "5\n")
        self.pm.boot()
        self.assertEqual(self.read("mode"), "0")
        self.assertIsNone(self.pm.status()["saved"])
        self.assertEqual(self.read("persist/power-mode"), "5")

    def test_already_in_the_mode_changes_nothing(self):
        self.write("mode", "0\n")
        self.pm.boot()
        self.assertEqual(self.calls(), ["nvpmodel -q"])


if __name__ == "__main__":
    unittest.main()
