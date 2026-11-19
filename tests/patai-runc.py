#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-runc, with a folder standing in for a container's cgroup. It has to pass on
a unit's Python 3.8:

    python3 tests/patai-runc.py
"""
import copy
import importlib.machinery
import importlib.util
import os
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__)) if "__file__" in globals() else "/nonexistent"
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-runc")
if not os.path.exists(SCRIPT):
    SCRIPT = "/usr/local/sbin/patai-runc"

loader = importlib.machinery.SourceFileLoader("patai_runc", SCRIPT)
spec = importlib.util.spec_from_loader("patai_runc", loader)
runc = importlib.util.module_from_spec(spec)
loader.exec_module(runc)

# What Docker gives runc for a container with user 10001, cap_drop ALL, no-new-privileges and a private cgroupns.
SPEC = {
    "process": {
        "user": {"uid": 10001, "gid": 10001},
        "capabilities": {"bounding": [], "effective": [], "permitted": []},
        "noNewPrivileges": True,
    },
    "mounts": [
        {"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
        {"destination": "/sys/fs/cgroup", "type": "cgroup", "source": "cgroup",
         "options": ["ro", "nosuid", "noexec", "nodev"]},
    ],
    "linux": {"namespaces": [{"type": "mount"}, {"type": "network"}, {"type": "cgroup"}]},
}


class Arguments(unittest.TestCase):
    def test_subcommand_after_global_options(self):
        args = ["--root", "/run/docker/runtime-runc/moby", "--log", "/x/log.json", "--log-format", "json",
                "--systemd-cgroup", "create", "--bundle", "/b", "--pid-file", "/b/init.pid", "abc"]
        at = runc.subcommand(args)
        self.assertEqual(args[at], "create")
        self.assertEqual(runc.option(args[at + 1:], "--bundle", "-b"), "/b")
        self.assertEqual(runc.option(args[at + 1:], "--pid-file"), "/b/init.pid")

    def test_options_with_equals(self):
        args = ["--root=/r", "create", "--bundle=/b", "--pid-file=/p", "abc"]
        at = runc.subcommand(args)
        self.assertEqual(at, 1)
        self.assertEqual(runc.option(args[at + 1:], "--bundle", "-b"), "/b")

    def test_no_subcommand(self):
        self.assertIsNone(runc.subcommand(["--version"]))


class Writable(unittest.TestCase):
    def test_makes_the_cgroup_mount_writable(self):
        spec = copy.deepcopy(SPEC)
        self.assertEqual(runc.writable(spec), (10001, 10001))
        mount = [m for m in spec["mounts"] if m["destination"] == "/sys/fs/cgroup"][0]
        self.assertEqual(mount["options"], ["rw", "nosuid", "noexec", "nodev"])
        self.assertEqual(spec["mounts"][0]["options"], ["nosuid", "noexec", "nodev"])

    def refused(self, change):
        spec = copy.deepcopy(SPEC)
        change(spec)
        with self.assertRaises(runc.Refused):
            runc.writable(spec)
        return spec

    def test_refuses_root(self):
        self.refused(lambda s: s["process"]["user"].update(uid=0))
        self.refused(lambda s: s["process"]["user"].update(gid=0))
        self.refused(lambda s: s["process"]["user"].update(additionalGids=[0]))
        self.refused(lambda s: s["process"].pop("user"))

    def test_refuses_capabilities(self):
        self.refused(lambda s: s["process"]["capabilities"].update(bounding=["CAP_CHOWN"]))
        self.refused(lambda s: s["process"]["capabilities"].update(ambient=["CAP_SYS_ADMIN"]))

    def test_refuses_new_privileges(self):
        self.refused(lambda s: s["process"].update(noNewPrivileges=False))

    def test_refuses_the_hosts_cgroup_namespace(self):
        self.refused(lambda s: s["linux"].update(namespaces=[{"type": "mount"}]))
        self.refused(lambda s: s["linux"]["namespaces"][2].update(path="/proc/1/ns/cgroup"))

    def test_refuses_another_mount_there(self):
        def bind(spec):
            spec["mounts"][1] = {"destination": "/sys/fs/cgroup", "type": "bind", "source": "/sys/fs/cgroup",
                                 "options": ["rbind", "rw"]}
        self.refused(bind)
        self.refused(lambda s: s.update(mounts=s["mounts"][:1]))


class Delegate(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.own = os.path.join(self.dir.name, "system.slice", "docker-abc.scope")
        os.makedirs(self.own)

    def tearDown(self):
        self.dir.cleanup()

    def fake_kernel(self, controllers):
        """Folders made here get the files a cgroup has, as the kernel would make them."""
        with open(os.path.join(self.own, "cgroup.controllers"), "w") as f:
            f.write(controllers)
        mkdir = os.mkdir

        def make(path, *args):
            mkdir(path, *args)
            for name in ("cgroup.procs", "cgroup.subtree_control", "cgroup.max.descendants", "cgroup.max.depth"):
                open(os.path.join(path, name), "w").close()
        runc.os.mkdir = make
        self.addCleanup(setattr, runc.os, "mkdir", mkdir)

    def read(self, *parts):
        with open(os.path.join(self.own, *parts)) as f:
            return f.read()

    def test_delegates_below_its_own_cgroup(self):
        self.fake_kernel("cpuset cpu io memory hugetlb pids")
        uid, gid = os.getuid(), os.getgid()
        runc.delegate(self.own, 4242, uid, gid)
        self.assertEqual(self.read("delegate", "main", "cgroup.procs"), "4242")
        self.assertEqual(self.read("cgroup.subtree_control"), "+memory +pids +cpu")
        self.assertEqual(self.read("delegate", "cgroup.subtree_control"), "+memory +pids +cpu")
        self.assertEqual(self.read("delegate", "cgroup.max.descendants"), "64")
        self.assertEqual(os.stat(os.path.join(self.own, "delegate")).st_uid, uid)

    def test_cpu_only_where_there_is_one(self):
        self.fake_kernel("memory pids")
        runc.delegate(self.own, 1, os.getuid(), os.getgid())
        self.assertEqual(self.read("cgroup.subtree_control"), "+memory +pids")

    def test_refuses_without_memory_or_pids(self):
        self.fake_kernel("cpu memory")
        with self.assertRaises(runc.Refused):
            runc.delegate(self.own, 1, os.getuid(), os.getgid())
        self.assertFalse(os.path.exists(os.path.join(self.own, "delegate")))

    def test_membership(self):
        self.assertEqual(runc.membership("0::/system.slice/docker-abc.scope\n"),
                         "/sys/fs/cgroup/system.slice/docker-abc.scope")
        for text in ("0::/\n", "0::/a/../b\n", "1:cpu:/x\n"):
            with self.assertRaises(runc.Refused):
                runc.membership(text)


if __name__ == "__main__":
    unittest.main()
