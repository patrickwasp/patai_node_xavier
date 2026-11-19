#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-network, with stand-ins for networkd, ip, udev and systemd. The
stand-in networkd does what 245 does with the files, and can be told to leave an old address behind. It has
to pass on a unit's Python 3.8:

    python3 tests/patai-network.py
    scripts/ssh python3 - < tests/patai-network.py    (with the script in /usr/local/sbin)
"""
import fcntl
import importlib.machinery
import importlib.util
import json
import os
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__)) if "__file__" in globals() else "/nonexistent"
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-network")
WIRED = os.path.join(HERE, "..", "overlay/etc/systemd/network/20-wired.network")
if not os.path.exists(SCRIPT):
    SCRIPT, WIRED = "/usr/local/sbin/patai-network", "/etc/systemd/network/20-wired.network"

GBE1 = "platform-2490000.ethernet"
GBE2 = "platform-14100000.pcie-pci-0001:01:00.0"

# One file of state for all the stand-ins: each interface's addresses and default gateway.
COMMON = r"""#!/usr/bin/env python3
import json, os, sys
STATE = os.environ["STUB_STATE"]
def load():
    with open(STATE) as f:
        return json.load(f)
def dump(state):
    with open(STATE, "w") as f:
        json.dump(state, f)
with open(os.environ["STUB_LOG"], "a") as f:
    f.write(os.path.basename(sys.argv[0]) + " " + " ".join(sys.argv[1:]) + "\n")
args = sys.argv[1:]
"""

IP = COMMON + r"""
state = load()
def entry(name):
    info = state["links"][name]
    return {"ifname": name, "addr_info": [
        {"family": "inet", "local": a["local"], "prefixlen": a["prefixlen"], "scope": a.get("scope", "global"),
         **({"dynamic": True} if a.get("dynamic") else {})} for a in info["addrs"]]}
if args[:2] == ["-j", "addr"] and len(args) == 2:
    print(json.dumps([entry(name) for name in state["links"]]))
elif args[:4] == ["-j", "addr", "show", "dev"]:
    print(json.dumps([entry(args[4])] if args[4] in state["links"] else []))
elif args[:5] == ["-j", "route", "show", "default", "dev"]:
    gw = state["links"].get(args[5], {}).get("gateway")
    print(json.dumps([{"dst": "default", "gateway": gw}] if gw else []))
elif args[:2] == ["addr", "del"]:
    local, prefix = args[2].split("/")
    links = state["links"][args[4]]
    links["addrs"] = [a for a in links["addrs"] if not (a["local"] == local and str(a["prefixlen"]) == prefix)]
    dump(state)
"""

NETWORKCTL = COMMON + r"""
if os.path.exists(os.environ["STUB_NETWORKD_FAILS"]):
    print("Failed to reload network settings: Connection refused", file=sys.stderr)
    sys.exit(1)
if args[:1] == ["reconfigure"]:
    if os.path.exists(os.environ["STUB_RECONFIGURE_FAILS"]):
        print("Failed to reconfigure network interface eth0: Device busy", file=sys.stderr)
        sys.exit(1)
    if os.path.exists(os.environ["STUB_STUCK"]):
        sys.exit(0)
    state = load()
    name = args[1]
    link = state["links"][name]
    path = state["paths"][name]
    found = None
    for file in sorted(os.listdir(os.environ["STUB_LIVE"])):
        text = open(os.path.join(os.environ["STUB_LIVE"], file)).read()
        if f"Path={path}\n" in text:
            found = text
            break
    old = link["addrs"]
    if found and "Address=" in found:
        local, prefix = found.split("Address=")[1].split("\n")[0].split("/")
        link["addrs"] = [{"local": local, "prefixlen": int(prefix)}, {"local": "169.254.3.47", "prefixlen": 16,
                                                                       "scope": "link"}]
        link["gateway"] = found.split("Gateway=")[1].split("\n")[0] if "Gateway=" in found else ""
    else:
        link["addrs"] = [dict(state["dhcp"][name], dynamic=True)]
        link["gateway"] = state["dhcp_gateway"]
    if os.path.exists(os.environ["STUB_LINGER"]):
        link["addrs"] += [a for a in old if a.get("scope", "global") == "global" and a not in link["addrs"]]
    dump(state)
"""

UDEVADM = COMMON + r"""
name = os.path.basename(args[-1].split("=", 1)[1])
path = load()["paths"].get(name)
if path:
    print(f"ID_NET_NAME=x\nID_PATH={path}")
"""

RESOLVECTL = COMMON + r"""
print(f"Link 3 ({args[1]}): 172.16.1.2 172.16.0.1")
"""

SYSTEMD_RUN = COMMON + r"""
sys.exit(1 if os.path.exists(os.environ["STUB_RUN_FAILS"]) else 0)
"""

SYSTEMCTL = COMMON


def load_script():
    loader = importlib.machinery.SourceFileLoader("patai_network", SCRIPT)
    spec = importlib.util.spec_from_loader("patai_network", loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = self.root = self.tmp.name
        for name in ("bin", "live", "persist", "run", "sys/eth0/device", "sys/eth1/device", "sys/lo"):
            os.makedirs(os.path.join(root, name))
        for name, mac in (("eth0", "00:0c:8b:b4:14:36"), ("eth1", "48:b0:2d:3a:c2:98")):
            self.write(f"sys/{name}/carrier", "1\n")
            self.write(f"sys/{name}/speed", "1000\n")
            self.write(f"sys/{name}/address", mac + "\n")
        self.write("hostname", "patai-test\n")
        self.state = os.path.join(root, "state.json")
        self.set_state({
            "paths": {"eth0": GBE2, "eth1": GBE1},
            "dhcp": {"eth0": {"local": "172.16.4.26", "prefixlen": 16},
                     "eth1": {"local": "192.168.50.9", "prefixlen": 24}},
            "dhcp_gateway": "172.16.0.1",
            "links": {
                "eth0": {"addrs": [{"local": "172.16.4.26", "prefixlen": 16, "dynamic": True}], "gateway": "172.16.0.1"},
                "eth1": {"addrs": [], "gateway": ""},
                "docker0": {"addrs": [{"local": "172.17.0.1", "prefixlen": 16}], "gateway": ""},
                "br-abc": {"addrs": [{"local": "172.18.0.1", "prefixlen": 16}], "gateway": ""},
            },
        })
        self.log = os.path.join(root, "calls")
        os.environ.update(STUB_STATE=self.state, STUB_LOG=self.log, STUB_LIVE=f"{root}/live",
                          STUB_LINGER=f"{root}/linger", STUB_RUN_FAILS=f"{root}/run-fails",
                          STUB_NETWORKD_FAILS=f"{root}/networkd-fails",
                          STUB_RECONFIGURE_FAILS=f"{root}/reconfigure-fails", STUB_STUCK=f"{root}/stuck")
        for name, text in (("ip", IP), ("networkctl", NETWORKCTL), ("udevadm", UDEVADM), ("resolvectl", RESOLVECTL),
                           ("systemd-run", SYSTEMD_RUN), ("systemctl", SYSTEMCTL)):
            self.write(f"bin/{name}", text)
            os.chmod(f"{root}/bin/{name}", 0o755)
        self.path = os.environ["PATH"]
        os.environ["PATH"] = f"{root}/bin:{self.path}"

        self.net = net = load_script()
        net.SAVED = f"{root}/persist/network.json"
        net.LIVE = f"{root}/live"
        net.TRIAL = f"{root}/run/network-trial.json"
        net.LAST = f"{root}/run/network-last.json"
        net.LOCK = f"{root}/run/network.lock"
        net.HOSTNAME = f"{root}/hostname"
        net.SYS_NET = f"{root}/sys"
        net.SETTLE_S = 0.5
        self.clock = 1000.0
        net.boottime = lambda: self.clock

    def tearDown(self):
        os.environ["PATH"] = self.path
        self.tmp.cleanup()

    def write(self, name, text):
        with open(os.path.join(self.root, name), "w") as f:
            f.write(text)

    def set_state(self, state):
        with open(self.state, "w") as f:
            json.dump(state, f)

    def get_state(self):
        with open(self.state) as f:
            return json.load(f)

    def calls(self):
        try:
            with open(self.log) as f:
                return f.read().splitlines()
        except FileNotFoundError:
            return []

    def live(self, port):
        path = self.net.live_file(port)
        if not os.path.exists(path):
            return None
        with open(path) as f:
            return f.read()

    def saved(self):
        try:
            with open(self.net.SAVED) as f:
                return json.load(f)
        except FileNotFoundError:
            return None

    def last(self):
        return self.net.load(self.net.LAST)

    def eth0(self):
        return [f"{a['local']}/{a['prefixlen']}" for a in self.get_state()["links"]["eth0"]["addrs"]
                if a.get("scope", "global") == "global"]

    def start(self, port="gbe2", **setting):
        setting.setdefault("mode", "static")
        if setting["mode"] == "static":
            setting.setdefault("address", "172.16.250.26/16")
            setting.setdefault("gateway", "172.16.0.1")
        return self.net.try_setting(port, json.dumps(setting))


STATIC = {"mode": "static", "address": "172.16.250.26/16", "gateway": "172.16.0.1", "dns": ["172.16.1.2"]}


class Checking(Base):
    def refused(self, setting, words, port="gbe2"):
        with self.assertRaises(self.net.Refused) as caught:
            self.net.check(port, setting)
        self.assertIn(words, str(caught.exception))

    def test_a_static_setting_comes_back_tidied(self):
        got = self.net.check("gbe2", {"mode": "static", "address": "172.16.250.26/16", "gateway": "172.16.0.1",
                                      "dns": ["172.16.1.2", "1.1.1.1", "172.16.1.2"]})
        self.assertEqual(got, {"mode": "static", "address": "172.16.250.26/16", "gateway": "172.16.0.1",
                               "dns": ["172.16.1.2", "1.1.1.1"]})
        self.assertEqual(self.net.check("gbe1", {"mode": "static", "address": "192.168.7.5/24"})["gateway"], "")
        self.assertEqual(self.net.check("gbe1", {"mode": "dhcp"}), self.net.DHCP)

    def test_what_isnt_an_address_for_a_device_is_refused(self):
        self.refused({"mode": "static", "address": "172.16.250.26"}, "prefix length")
        self.refused({"mode": "static", "address": "172.16.250.300/16"}, "isn't an address")
        self.refused({"mode": "static", "address": "172.16.250.26/31"}, "between 8 and 30")
        self.refused({"mode": "static", "address": "172.16.250.26/7"}, "between 8 and 30")
        self.refused({"mode": "static", "address": "192.168.7.0/24"}, "address of 192.168.7.0/24 itself")
        self.refused({"mode": "static", "address": "192.168.7.255/24"}, "itself")
        for bad in ("127.0.0.5/8", "169.254.9.9/16", "224.0.0.9/8", "240.1.1.1/8", "0.1.2.3/8"):
            self.refused({"mode": "static", "address": bad}, "can't be")

    def test_a_gateway_has_to_be_on_the_network(self):
        self.refused({"mode": "static", "address": "192.168.7.5/24", "gateway": "192.168.8.1"}, "has to be on")
        self.refused({"mode": "static", "address": "192.168.7.5/24", "gateway": "192.168.7.5"}, "own address")
        self.refused({"mode": "static", "address": "192.168.7.5/24", "gateway": "192.168.7.255"}, "has to be on")

    def test_dns_and_the_shape_are_checked(self):
        self.refused({"mode": "dhcp", "dns": ["1.1.1.1"] * 4}, "at most 3")
        self.refused({"mode": "dhcp", "dns": ["dns.example"]}, "isn't an IPv4 address")
        self.refused({"mode": "dhcp", "dns": "1.1.1.1"}, "at most 3")
        self.refused({"mode": "dhcp", "address": "10.0.0.5/24"}, "network gives")
        self.refused({"mode": "bridge"}, "either DHCP or static")
        self.refused({"mode": "dhcp", "mtu": 9000}, "isn't a network setting")
        self.refused({"mode": "dhcp"}, "no port called", port="eth0")


class Rendering(Base):
    def test_a_static_port_keeps_mdns_and_link_local(self):
        text = self.net.render("gbe2", STATIC)
        for line in (f"Path={GBE2}", "MulticastDNS=yes", "LinkLocalAddressing=yes", "Address=172.16.250.26/16",
                     "DNS=172.16.1.2", "Gateway=172.16.0.1", "Metric=1024"):
            self.assertIn(line + "\n", text)
        self.assertNotIn("DHCP=", text)
        self.assertNotIn("[Route]", self.net.render("gbe1", {**STATIC, "gateway": ""}))

    def test_dhcp_with_its_own_dns_is_the_shipped_file_plus_that(self):
        text = self.net.render("gbe1", {"mode": "dhcp", "dns": ["1.1.1.1"]})
        with open(WIRED) as f:
            shipped = [line for line in f.read().splitlines()
                       if "=" in line and not line.startswith("#") and not line.startswith("Name=")]
        for line in shipped:
            self.assertIn(line + "\n", text)
        self.assertIn("DNS=1.1.1.1\n", text)
        self.assertIn("UseDNS=no\n", text)
        self.assertIn(f"Path={GBE1}\n", text)


class Trial(Base):
    def test_a_kept_change_is_saved(self):
        trial = self.start(dns=["172.16.1.2"])
        self.assertFalse(trial["applied"])
        self.assertEqual(trial["urls"], ["https://172.16.250.26/", "https://patai-test.local/"])
        self.assertEqual(trial["left_s"], self.net.SWITCH_S + self.net.TRIAL_S)
        runs = [line for line in self.calls() if line.startswith("systemd-run")]
        self.assertIn("-revert", runs[0], "the way back is set up before the change")
        self.assertIn("--on-boot=1302", runs[0], "counted from boot, which a daemon-reload doesn't move")
        self.assertIn("Restart=on-failure", runs[0])
        self.assertIn(f"revert {trial['id']}", runs[0])
        self.assertIn("--on-boot=1002", runs[1])
        self.assertIsNone(self.live("gbe2"), "nothing changes until the switch")

        self.net.switch(trial["id"])
        self.assertIn("Address=172.16.250.26/16", self.live("gbe2"))
        self.assertIn("networkctl reconfigure eth0", self.calls())
        self.assertEqual(self.eth0(), ["172.16.250.26/16"])
        self.assertIsNone(self.saved(), "nothing is saved until it's kept")

        self.net.keep(trial["id"])
        self.assertEqual(self.saved(), {"version": 1, "ports": {"gbe2": STATIC}})
        self.assertFalse(os.path.exists(self.net.TRIAL))
        self.assertEqual(self.last()["outcome"], "kept")
        self.assertIn(f"systemctl stop patai-network-{trial['id']}-revert.timer", self.calls())

    def test_a_change_nobody_keeps_goes_back(self):
        trial = self.start()
        self.net.switch(trial["id"])
        self.clock += 302
        self.net.revert(trial["id"], "went back")
        self.assertIsNone(self.live("gbe2"))
        self.assertEqual(self.eth0(), ["172.16.4.26/16"])
        self.assertIsNone(self.saved())
        self.assertEqual(self.last()["outcome"], "went back")
        with self.assertRaises(self.net.Refused) as caught:
            self.net.keep(trial["id"])
        self.assertIn("went back", str(caught.exception))

    def test_going_back_returns_to_what_was_saved_before(self):
        self.net.save({"gbe2": STATIC})
        self.net.apply()
        trial = self.start(address="172.16.250.27/16")
        self.net.switch(trial["id"])
        self.assertIn("Address=172.16.250.27/16", self.live("gbe2"))
        self.net.revert(trial["id"], "went back")
        self.assertIn("Address=172.16.250.26/16", self.live("gbe2"))
        self.assertEqual(self.eth0(), ["172.16.250.26/16"])

    def test_undo_before_the_switch_changes_nothing(self):
        trial = self.start()
        self.net.revert(trial["id"], "undone")
        self.assertNotIn("networkctl reload", self.calls())
        self.net.switch(trial["id"])
        self.assertIsNone(self.live("gbe2"), "a switch that comes late does nothing")
        self.assertEqual(self.last()["outcome"], "undone")
        with self.assertRaises(self.net.Refused):
            self.net.revert(trial["id"], "undone")

    def test_keep_has_to_be_in_time_and_for_this_trial(self):
        trial = self.start()
        with self.assertRaises(self.net.Refused) as caught:
            self.net.keep(trial["id"])
        self.assertIn("hasn't been made yet", str(caught.exception))
        self.net.switch(trial["id"])
        with self.assertRaises(self.net.Refused):
            self.net.keep("0123456789abcdef")
        self.clock += 303
        with self.assertRaises(self.net.Refused) as caught:
            self.net.keep(trial["id"])
        self.assertIn("too late", str(caught.exception))
        self.assertIsNone(self.saved())

    def test_one_trial_at_a_time_and_no_empty_changes(self):
        self.start()
        with self.assertRaises(self.net.Refused) as caught:
            self.start(port="gbe1", address="192.168.60.5/24", gateway="")
        self.assertIn("waiting to be kept", str(caught.exception))
        os.remove(self.net.TRIAL)
        with self.assertRaises(self.net.Refused) as caught:
            self.start(mode="dhcp")
        self.assertIn("set like that already", str(caught.exception))

    def test_an_overdue_trial_is_ended_by_the_next_look(self):
        trial = self.start()
        self.net.switch(trial["id"])
        self.clock += 302 + self.net.GRACE_S + 1
        status = self.net.status()
        self.assertIsNone(status["trial"])
        self.assertEqual(status["last"]["outcome"], "went back")
        self.assertIsNone(self.live("gbe2"))

    def test_a_look_doesnt_wait_for_the_lock(self):
        trial = self.start()
        with open(self.net.LOCK, "w") as held:
            fcntl.flock(held, fcntl.LOCK_EX)
            began = time.monotonic()
            status = self.net.status()
            self.assertLess(time.monotonic() - began, 5)
        self.assertEqual(status["trial"]["id"], trial["id"])

    def test_no_timer_means_no_change(self):
        self.write("run-fails", "")
        with self.assertRaises(self.net.Refused):
            self.start()
        self.assertFalse(os.path.exists(self.net.TRIAL))

    def test_a_port_the_unit_lacks_is_refused(self):
        state = self.get_state()
        del state["paths"]["eth1"]
        self.set_state(state)
        with self.assertRaises(self.net.Refused) as caught:
            self.start(port="gbe1", address="192.168.60.5/24", gateway="")
        self.assertIn("no GbE 1", str(caught.exception))

    def test_a_failed_way_back_is_left_to_try_again(self):
        trial = self.start()
        self.net.switch(trial["id"])
        self.write("networkd-fails", "")
        with self.assertRaises(self.net.Refused):
            self.net.revert(trial["id"], "went back")
        self.assertTrue(os.path.exists(self.net.TRIAL), "the trial stays until it's really over")
        os.remove(f"{self.root}/networkd-fails")
        self.net.revert(trial["id"], "went back")
        self.assertEqual(self.eth0(), ["172.16.4.26/16"])


class Failures(Base):
    def test_a_failed_undo_leaves_the_timer_to_end_it(self):
        trial = self.start()
        self.net.switch(trial["id"])
        self.write("networkd-fails", "")
        with self.assertRaises(self.net.Refused):
            self.net.revert(trial["id"], "undone")
        self.assertNotIn(f"systemctl stop patai-network-{trial['id']}-revert.timer", self.calls())
        self.assertTrue(os.path.exists(self.net.TRIAL))

    def test_a_change_on_its_way_back_cant_be_kept(self):
        trial = self.start()
        self.net.switch(trial["id"])
        self.write("reconfigure-fails", "")
        with self.assertRaises(self.net.Refused):
            self.net.revert(trial["id"], "undone")
        self.assertTrue(self.net.status()["trial"]["reverting"])
        with self.assertRaises(self.net.Refused) as caught:
            self.net.keep(trial["id"])
        self.assertIn("going back already", str(caught.exception))
        self.assertIsNone(self.saved())
        os.remove(f"{self.root}/reconfigure-fails")
        self.net.revert(trial["id"], "went back")
        self.assertEqual(self.eth0(), ["172.16.4.26/16"])

    def test_a_failed_reconfigure_is_a_failure(self):
        self.write("reconfigure-fails", "")
        trial = self.start()
        with self.assertRaises(self.net.Refused) as caught:
            self.net.switch(trial["id"])
        self.assertIn("Device busy", str(caught.exception))
        self.assertTrue(self.net.read_trial()["applied"], "a change half made is one to undo")

    def test_the_way_back_takes_nothing_away_until_the_saved_address_is_there(self):
        self.net.save({"gbe2": STATIC})
        self.net.apply()
        trial = self.start(address="172.16.250.27/16")
        self.net.switch(trial["id"])
        self.write("stuck", "")
        with self.assertRaises(self.net.Refused) as caught:
            self.net.revert(trial["id"], "went back")
        self.assertIn("172.16.250.26/16 didn't come up", str(caught.exception))
        self.assertEqual(self.eth0(), ["172.16.250.27/16"], "the address that works stays")
        self.assertTrue(os.path.exists(self.net.TRIAL))
        os.remove(f"{self.root}/stuck")
        self.net.revert(trial["id"], "went back")
        self.assertEqual(self.eth0(), ["172.16.250.26/16"])

    def test_dhcp_that_doesnt_answer_still_takes_the_static_address_away(self):
        self.net.save({"gbe2": STATIC})
        self.net.apply()
        state = self.get_state()
        state["links"]["eth0"]["addrs"] = [{"local": "172.16.250.26", "prefixlen": 16}]
        self.set_state(state)
        self.write("stuck", "")
        trial = self.start(mode="dhcp")
        self.net.switch(trial["id"])
        self.assertEqual(self.eth0(), [], "DHCP is what was asked for, answered or not")


class LeftBehind(Base):
    def test_an_old_dhcp_address_is_taken_away(self):
        self.write("linger", "")
        trial = self.start()
        self.net.switch(trial["id"])
        self.assertEqual(self.eth0(), ["172.16.250.26/16"])
        self.assertIn("ip addr del 172.16.4.26/16 dev eth0", self.calls())

    def test_an_old_static_address_is_taken_away(self):
        self.net.save({"gbe2": STATIC})
        self.net.apply()
        self.calls()
        state = self.get_state()
        state["links"]["eth0"]["addrs"] = [{"local": "172.16.250.26", "prefixlen": 16}]
        self.set_state(state)
        self.write("linger", "")
        trial = self.start(mode="dhcp")
        self.net.switch(trial["id"])
        self.assertEqual(self.eth0(), ["172.16.4.26/16"])


class Clashes(Base):
    def refused(self, words, **setting):
        with self.assertRaises(self.net.Refused) as caught:
            self.start(**setting)
        self.assertIn(words, str(caught.exception))

    def test_docker_networks_are_refused(self):
        self.refused("which Docker uses", address="172.17.5.5/24", gateway="")
        self.refused("which Docker uses", address="172.16.0.5/12", gateway="")

    def test_tailscales_addresses_are_refused(self):
        self.refused("which Tailscale uses", address="100.100.4.2/24", gateway="")

    def test_the_other_ports_network_is_refused(self):
        self.refused("GbE 2 is on 172.16.0.0/16", port="gbe1", address="172.16.9.5/24", gateway="")
        state = self.get_state()
        state["links"]["eth0"]["addrs"] = []
        self.set_state(state)
        self.net.save({"gbe2": STATIC})
        self.refused("GbE 2 is on 172.16.0.0/16", port="gbe1", address="172.16.9.5/24", gateway="")
        self.assertEqual(self.start(port="gbe1", address="192.168.60.5/24", gateway="")["port"], "gbe1")


class Saved(Base):
    def test_boot_writes_what_was_saved(self):
        self.net.save({"gbe1": {"mode": "dhcp", "dns": ["1.1.1.1"]}, "gbe2": STATIC})
        self.net.apply()
        self.assertIn("UseDNS=no", self.live("gbe1"))
        self.assertIn("Address=172.16.250.26/16", self.live("gbe2"))
        self.net.save({})
        self.assertIsNone(self.saved(), "plain DHCP everywhere is no file at all")
        self.net.apply()
        self.assertIsNone(self.live("gbe2"))

    def test_a_setting_that_cant_be_used_leaves_dhcp_and_says_so(self):
        self.write("persist/network.json", json.dumps({"version": 1, "ports": {
            "gbe2": {"mode": "static", "address": "172.16.250.26/33"}, "gbe1": {"mode": "dhcp", "dns": ["1.1.1.1"]}}}))
        self.net.apply()
        self.assertIsNone(self.live("gbe2"))
        self.assertIn("UseDNS=no", self.live("gbe1"))
        self.assertIn("GbE 2's saved setting can't be used", self.net.status()["problem"])
        self.write("persist/network.json", "{not json")
        self.net.apply()
        self.assertIn("can't be read", self.net.status()["problem"])
        self.assertIsNone(self.live("gbe1"))

    def test_a_port_this_release_doesnt_know_is_kept(self):
        self.write("persist/network.json", json.dumps({"version": 1, "ports": {"wifi": {"mode": "dhcp", "ssid": "x"}}}))
        trial = self.start(dns=["172.16.1.2"])
        self.net.switch(trial["id"])
        self.net.keep(trial["id"])
        self.assertEqual(self.saved()["ports"], {"wifi": {"mode": "dhcp", "ssid": "x"}, "gbe2": STATIC})

    def test_reset_goes_back_to_dhcp(self):
        self.net.save({"gbe2": STATIC})
        self.net.apply()
        trial = self.start(address="172.16.250.27/16")
        self.net.switch(trial["id"])
        self.net.reset()
        self.assertIsNone(self.saved())
        self.assertIsNone(self.live("gbe2"))
        self.assertFalse(os.path.exists(self.net.TRIAL))
        self.assertEqual(self.eth0(), ["172.16.4.26/16"])


class Status(Base):
    def test_status_says_what_each_port_is(self):
        self.net.save({"gbe2": STATIC})
        status = self.net.status()
        gbe1, gbe2 = status["ports"]
        self.assertEqual((gbe1["id"], gbe1["label"], gbe1["connector"], gbe1["interface"]),
                         ("gbe1", "GbE 1", "J1A", "eth1"))
        self.assertEqual((gbe2["interface"], gbe2["mac"], gbe2["speed_mbps"]), ("eth0", "00:0c:8b:b4:14:36", 1000))
        self.assertEqual(gbe2["addresses"], ["172.16.4.26/16"])
        self.assertEqual(gbe2["gateway"], "172.16.0.1")
        self.assertEqual(gbe2["dns"], ["172.16.1.2", "172.16.0.1"])
        self.assertEqual(gbe2["saved"], STATIC)
        self.assertEqual(gbe1["saved"], self.net.DHCP)
        self.assertEqual(status["problem"], "")
        self.assertEqual(status["trial_s"], 300)

    def test_status_shows_a_trial_counting_down(self):
        trial = self.start()
        self.clock += 100
        shown = self.net.status()["trial"]
        self.assertEqual((shown["id"], shown["left_s"]), (trial["id"], 202))

    def test_main_reports_refusals_in_words(self):
        argv = sys.argv
        self.assertEqual(self.net.main(["try", "gbe2", "{"]), 1)
        self.assertEqual(self.net.main(["frobnicate"]), 2)
        sys.argv = argv


if __name__ == "__main__":
    unittest.main(verbosity=1)
