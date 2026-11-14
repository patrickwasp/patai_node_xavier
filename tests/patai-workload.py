#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-workload, with stand-ins for docker, systemctl and patai-fetch. It has
to pass on a unit's Python 3.8:

    python3 tests/patai-workload.py
    scripts/ssh python3 - < tests/patai-workload.py    (with the script in /usr/local/sbin)
"""
import hashlib
import importlib.machinery
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__)) if "__file__" in globals() else "/nonexistent"
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-workload")
if not os.path.exists(SCRIPT):
    SCRIPT = "/usr/local/sbin/patai-workload"

# Logs each call with the settings Compose would see, and answers from files under its root. Compose gets none of
# this process's environment, so the paths are written in. For each project, "containers-<project>" holds what
# exists, container-<project> if anything, and "running-<project>" what its inspect says: its image and mounts.
DOCKER = """#!/bin/sh
root={root}
echo "docker $* LLAMA_CONTEXT=${{LLAMA_CONTEXT:-}} LLAMA_MODEL=${{LLAMA_MODEL:-}} TEMP=${{TEMP:-}} CACHE=${{CACHE:-}} KEY=${{KEY:-}} MADE=${{MADE:-}} ONCE=${{ONCE:-}} GW=${{GW:-}} DB=${{DB:-}} APIS=${{PATAI_APIS:-}}" >> "$root/calls"
for last; do :; done
project=$(echo "$*" | sed -n 's/.* -p \\([^ ]*\\) .*/\\1/p')
case "$1 $2 $3" in
  "ps -a -q") cat "$root/containers-${{last#label=com.docker.compose.project=}}" 2>/dev/null || true ;;
  "ps -a --format") cat "$root/projects" 2>/dev/null || true ;;
  inspect*) cat "$root/running-${{last#container-}}" ;;
  compose*)
    case "$*" in
      *" up "*) [ ! -e "$root/up-fails" ] || {{ echo "no luck" >&2; exit 1; }}; echo "container-$project" > "$root/containers-$project" ;;
      *" down "*) rm -f "$root/containers-$project" ;;
      *" start "*) [ ! -e "$root/start-fails" ] && [ ! -e "$root/start-fails-$project" ] || {{ echo "no luck" >&2; exit 1; }} ;;
    esac
    ;;
esac
"""
OTHER = """#!/bin/sh
echo "$(basename "$0") $*" >> "{root}/calls"
[ "$(basename "$0") $1" != "systemctl stop" ] || echo "models.json at the stop: $(cat {root}/provision/models.json 2>/dev/null)" >> "{root}/calls"
[ "$(basename "$0")" != patai-fetch ] || [ ! -e "{root}/fetch-changes" ] || cp "{root}/fetch-changes" "{root}/provision/models.json"
if [ "$(basename "$0") $1" = "systemctl is-active" ]; then
  case "$*" in *docker*) [ ! -e "{root}/docker-active" ] || {{ echo active; exit 0; }} ;; esac
  case "$*" in *--quiet*) ;; *) echo inactive ;; esac
  exit 3
fi
[ "$(basename "$0") $1" != "patai-fetch llama" ] || [ ! -e "{root}/fetch-fails" ] || exit 1
"""
COMPOSE = "services:\n  server:\n    image: x\n"


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = root = self.tmp.name
        for name in ("bin", "recipes", "provision", "run"):
            os.makedirs(os.path.join(root, name))
        for name, text in (("docker", DOCKER), ("systemctl", OTHER), ("systemd-run", OTHER), ("patai-fetch", OTHER)):
            self.write(f"bin/{name}", text.format(root=root))
            os.chmod(f"{root}/bin/{name}", 0o755)
        self.path = os.environ["PATH"]
        os.environ["PATH"] = f"{root}/bin:{self.path}"
        self.app("llama", env={"LLAMA_CONTEXT": "8192"}, models=[("a.gguf", "", True), ("b.gguf", "", False)],
                 block={"env": "LLAMA_MODEL", "dir": "", "formats": ["gguf"]})
        self.app("comfy", models=[("sd.safetensors", "checkpoints", True)],
                 block={"dir": "checkpoints", "formats": ["safetensors"]})
        self.write("recipes/yolo/compose.yaml", "services:\n  bench:\n    image: x\n")
        self.write("recipes/yolo/manifest.json", json.dumps({"name": "yolo"}))
        self.write("apps/platform/manifest.json", json.dumps({"name": "platform", "apis": [
            {"name": "chat", "service": "llama", "always": True}, {"name": "images", "service": "comfy"}]}))
        self.write("apps/studio/manifest.json", json.dumps({"name": "studio", "services": ["comfy"]}))
        self.running("sha256:llama", gated="llama")
        self.running("sha256:comfy", gated="comfy", app="comfy")

        loader = importlib.machinery.SourceFileLoader("patai_workload", SCRIPT)
        spec = importlib.util.spec_from_loader("patai_workload", loader)
        self.w = importlib.util.module_from_spec(spec)
        loader.exec_module(self.w)
        w = self.w
        w.RECIPES = f"{root}/recipes"
        w.APPS = f"{root}/apps"
        w.DESIRED = f"{root}/provision/apps.json"
        w.SECRETS = f"{root}/provision/secrets.json"
        w.DATA = f"{root}/data"
        w.MODELS_STATE = f"{root}/provision/models.json"
        w.MODELS = f"{root}/models"
        w.GATES = f"{root}/run/apps"
        w.LOCK = f"{root}/run/workload.lock"
        w.COMMAND_LOCK = f"{root}/run/workload-command.lock"
        w.FETCH = f"{root}/bin/patai-fetch"
        w.FETCH_LOCK = f"{root}/run/fetch.lock"
        w.CONTAINERS = f"{root}/docker/containers"
        w.ENV = {**w.ENV, "PATH": f"{root}/bin:{w.ENV['PATH']}"}
        w.os.path.ismount = lambda path: True

    def tearDown(self):
        os.environ["PATH"] = self.path
        self.tmp.cleanup()

    def write(self, name, data):
        path = os.path.join(self.root, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(data)
        return path

    def app(self, name, env=None, models=(), block=None, settings=None):
        """A recipe, with its pinned models as (name, folder, default[, env[, its settings' defaults and limits]]),
        how it takes them, and the settings it declares."""
        pinned = [{"name": model, "dir": folder, "default": default, "sha256": "0" * 64, "size": 1, "url": "https://x/y",
                   **({"env": rest[0]} if rest else {}), **(rest[1] if len(rest) > 1 else {})}
                  for model, folder, default, *rest in models]
        self.write(f"recipes/{name}/compose.yaml", COMPOSE)
        self.write(f"recipes/{name}/manifest.json", json.dumps({
            "name": name, "ready": f"http://127.0.0.1:1/{name}", "image": {"id": f"sha256:{name}"}, "profile": "standard",
            "profiles": {"standard": {"env": env or {}, "memory_mib": 1}}, "models": pinned,
            **({"model": block} if block else {}), **({"settings": settings} if settings else {})}))

    def models(self):
        with open(self.w.MODELS_STATE) as f:
            return json.load(f)

    def running(self, image, gated=None, app="llama"):
        """What the app's container's inspect says: its image, and the gate it mounts, if any."""
        self.write(f"running-{app}", f"{image} /data/models/x" + (f" {self.root}/run/apps/{gated}" if gated else "") + "\n")

    def container_there(self, app="llama"):
        self.write(f"containers-{app}", f"container-{app}\n")

    def choose(self, *apps, images=False):
        """Apps on: images=True turns that API on for platform."""
        self.w.choose({"chosen": list(apps), "apis": {"platform": ["chat", "images"]} if images else {}, "settings": {}})

    def run_it(self, *args):
        argv, stdout, stderr = sys.argv, sys.stdout, sys.stderr
        sys.argv = ["patai-workload", *args]
        sys.stdout, sys.stderr = io.StringIO(), io.StringIO()
        try:
            self.w.main()
            return None
        except SystemExit as stop:
            return stop.code
        finally:
            self.out = sys.stdout.getvalue()
            sys.argv, sys.stdout, sys.stderr = argv, stdout, stderr

    def calls(self):
        try:
            with open(f"{self.root}/calls") as f:
                return f.read()
        except OSError:
            return ""

    def before(self, first, then):
        calls = self.calls()
        self.assertIn(first, calls)
        self.assertIn(then, calls)
        self.assertLess(calls.index(first), calls.index(then), calls)

    def state(self):
        with open(self.w.DESIRED) as f:
            return json.load(f)

    def gate(self, app):
        return os.path.isdir(f"{self.w.GATES}/{app}")


class Desired(Base):
    NONE = {"chosen": [], "apis": {}, "settings": {}}

    def test_nothing_chosen(self):
        self.assertEqual(self.w.desired(), self.NONE)

    def test_a_choice_is_kept(self):
        self.choose("platform", images=True)
        self.assertEqual(self.w.desired(), {"chosen": ["platform"], "apis": {"platform": ["chat", "images"]}, "settings": {}})

    def test_a_damaged_file_chooses_nothing(self):
        for text in ("", "{", "[]", '{"chosen": 3}', '{"chosen": {"platform": 1}}'):
            self.write("provision/apps.json", text)
            self.assertEqual(self.w.desired(), self.NONE, text)

    def test_odd_names_and_unknown_apps_are_dropped(self):
        self.write("provision/apps.json", json.dumps({"chosen": ["../x", "platform", 3, "platform", "newer"],
                                                      "apis": {"platform": ["images", 3], "x": 3, "newer": ["a"]},
                                                      "settings": {"llama": {"a.gguf": {"x": 1}, "b.gguf": 3},
                                                                   "newer": {}, "comfy": 3}}))
        self.assertEqual(self.w.desired(), {"chosen": ["platform"], "apis": {"platform": ["images"]},
                                            "settings": {"llama": {"a.gguf": {"x": 1}}}})

    def test_no_persist_means_no_choice(self):
        self.w.os.path.ismount = lambda path: False
        self.assertIsNotNone(self.run_it("start", "platform"))
        self.assertFalse(os.path.exists(self.w.DESIRED))


class Serving(Base):
    def test_nothing_chosen_starts_nothing(self):
        self.assertIsNone(self.run_it("serve"))
        self.assertNotIn("docker compose", self.calls())

    def test_made_checked_then_started(self):
        self.choose("platform")
        self.assertIsNone(self.run_it("serve"))
        self.before("patai-fetch llama", f"compose -f {self.root}/recipes/llama/compose.yaml -p llama up --no-start --no-build server")
        self.before("up --no-start", "docker inspect")
        self.before("docker inspect", "-p llama start server")
        self.assertIn("LLAMA_CONTEXT=8192 LLAMA_MODEL=a.gguf", self.calls())
        self.assertTrue(self.gate("llama"))
        self.assertEqual(self.state()["chosen"], ["platform"])

    def test_compose_gets_only_the_profiles_settings(self):
        self.choose("platform")
        os.environ["LLAMA_MODEL"] = "something-unchecked.gguf"
        try:
            self.assertIsNone(self.run_it("serve"))
        finally:
            del os.environ["LLAMA_MODEL"]
        composed = [line for line in self.calls().splitlines() if line.startswith("docker compose")]
        self.assertTrue(composed)
        self.assertFalse([line for line in composed if "something-unchecked" in line])

    def test_a_failed_fetch_leaves_the_gate_shut(self):
        self.choose("platform")
        self.write("fetch-fails", "")
        self.assertIsNotNone(self.run_it("serve"))
        self.assertNotIn(" up ", self.calls())
        self.assertFalse(self.gate("llama"))

    def test_a_container_without_its_gate_is_taken_down_before_fetching(self):
        self.choose("platform")
        self.container_there()
        self.running("sha256:llama")
        self.write("fetch-fails", "")
        self.assertIsNotNone(self.run_it("serve"))
        self.before("-p llama down --remove-orphans", "patai-fetch llama")

    def test_a_container_of_another_image_is_taken_down_before_fetching(self):
        self.choose("platform")
        self.container_there()
        self.running("sha256:older", gated="llama")
        self.write("fetch-fails", "")
        self.assertIsNotNone(self.run_it("serve"))
        self.before("-p llama down --remove-orphans", "patai-fetch llama")

    def test_a_checked_container_is_kept(self):
        self.choose("platform")
        self.container_there()
        self.assertIsNone(self.run_it("serve"))
        self.assertNotIn("down", self.calls())

    def test_the_wrong_image_is_removed_before_it_starts(self):
        self.choose("platform")
        self.running("sha256:something-else", gated="llama")
        self.assertIsNotNone(self.run_it("serve"))
        calls = self.calls()
        self.before("up --no-start", "-p llama down --remove-orphans")
        self.assertNotIn(" start server", calls)
        self.assertIn("patai-fetch --failed llama its container wasn't made from the image", calls)
        self.assertFalse(self.gate("llama"))

    def test_a_container_without_its_gate_isnt_started(self):
        self.choose("platform")
        self.running("sha256:llama")
        self.assertIsNotNone(self.run_it("serve"))
        self.assertNotIn(" start server", self.calls())

    def test_a_container_that_cant_be_made_is_a_failure(self):
        self.choose("platform")
        self.write("up-fails", "")
        self.assertIsNotNone(self.run_it("serve"))
        self.assertIn("patai-fetch --failed llama its container couldn't be made", self.calls())

    def test_a_container_that_wont_start_is_a_failure(self):
        self.choose("platform")
        self.write("start-fails", "")
        self.assertIsNotNone(self.run_it("serve"))
        self.assertIn("patai-fetch --failed llama its container wouldn't start", self.calls())

    def test_others_are_taken_down_and_their_gates_shut(self):
        self.choose("platform")
        self.write("projects", "comfy\nllama\nother-app\n\n")
        os.makedirs(f"{self.w.GATES}/comfy")
        self.assertIsNone(self.run_it("serve"))
        calls = self.calls()
        self.assertIn(f"compose -f {self.root}/recipes/comfy/compose.yaml -p comfy down --remove-orphans", calls)
        # This release has no recipe for it, and its name is enough.
        self.assertIn("compose -p other-app down --remove-orphans", calls)
        self.assertNotIn("-p llama down", calls)
        self.assertFalse(self.gate("comfy"))

    def test_only_what_compose_made_counts(self):
        # The app's image carries Compose's project label, so a container run from it by hand has it too.
        self.choose("platform")
        self.assertIsNone(self.run_it("serve"))
        listed = [line for line in self.calls().splitlines() if line.startswith("docker ps -a -q")]
        self.assertTrue(listed)
        self.assertTrue(all("--filter label=com.docker.compose.oneoff=False" in line for line in listed), listed)

    def test_a_project_name_isnt_a_path(self):
        self.write("projects", "../../etc\n")
        self.assertIsNone(self.run_it("serve"))
        self.assertNotIn("down", self.calls())

    def test_an_app_this_release_doesnt_have_stays_stopped(self):
        self.write("provision/apps.json", json.dumps({"chosen": ["other"]}))
        self.write("projects", "other\n")
        self.assertIsNone(self.run_it("serve"))
        calls = self.calls()
        self.assertIn("compose -p other down --remove-orphans", calls)
        self.assertNotIn(" up ", calls)
        self.assertNotIn("patai-fetch other", calls)

    def test_every_service_the_apps_need_in_their_order(self):
        self.choose("platform", images=True)
        self.assertIsNone(self.run_it("serve"))
        self.before("-p llama start server", "patai-fetch comfy")
        self.assertIn("-p comfy start server", self.calls())
        self.assertTrue(self.gate("llama") and self.gate("comfy"))

    def test_a_service_two_apps_share_runs_once(self):
        self.choose("studio", "platform", images=True)
        self.assertIsNone(self.run_it("serve"))
        self.assertEqual(self.calls().count("-p comfy start server"), 1)

    def test_one_that_fails_doesnt_hold_up_the_rest(self):
        self.choose("platform", images=True)
        self.write("start-fails-llama", "")
        self.assertIn("llama", str(self.run_it("serve")))
        self.assertIn("-p comfy start server", self.calls())
        self.write("fetch-fails", "")
        self.assertIsNotNone(self.run_it("serve"))

    def test_a_recipe_without_a_server_isnt_served(self):
        self.write("apps/detector/manifest.json", json.dumps({"name": "detector", "services": ["yolo"]}))
        self.choose("detector")
        self.assertIsNone(self.run_it("serve"))
        self.assertNotIn(" up ", self.calls())

    def test_a_profile_setting_that_cant_be_passed_on(self):
        self.app("llama", env={"LLAMA_CONTEXT": "8192; reboot"})
        self.choose("platform")
        self.assertIsNotNone(self.run_it("serve"))
        self.assertNotIn(" up ", self.calls())

    def test_a_pinned_models_settings_go_over_its_profiles(self):
        self.app("llama", env={"LLAMA_CONTEXT": "8192"}, models=[("a.gguf", "", True, {"LLAMA_CONTEXT": "131072"}),
                 ("b.gguf", "", False)], block={"env": "LLAMA_MODEL", "dir": "", "formats": ["gguf"]})
        self.choose("platform")
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("LLAMA_CONTEXT=131072 LLAMA_MODEL=a.gguf", self.calls())
        self.assertIsNone(self.run_it("model", "use", "llama", "b.gguf"))
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("LLAMA_CONTEXT=8192 LLAMA_MODEL=b.gguf", self.calls())

    def test_a_model_setting_that_cant_be_passed_on(self):
        self.app("llama", models=[("a.gguf", "", True, {"LLAMA_CONTEXT": "8192 -m /etc/shadow"})],
                 block={"env": "LLAMA_MODEL", "dir": "", "formats": ["gguf"]})
        self.choose("platform")
        self.assertIsNotNone(self.run_it("serve"))
        self.assertNotIn(" up ", self.calls())

    def test_an_api_turned_off_has_its_service_taken_down(self):
        self.choose("platform")
        self.write("projects", "comfy\nllama\n")
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("-p comfy down --remove-orphans", self.calls())
        self.assertNotIn("-p llama down", self.calls())


class Holding(Base):
    """What Docker would read when it starts, in the files it keeps for each container."""

    def container(self, name, app, gated, policy="on-failure"):
        folder = f"docker/containers/{name}"
        labels = {"com.docker.compose.project": app} if app else {}
        mounts = {"/models": {"Source": "/data/models/x"}}
        if gated:
            mounts["/run/patai-gate"] = {"Source": f"{self.root}/run/apps/{app}"}
        self.write(f"{folder}/config.v2.json", json.dumps({"Config": {"Labels": labels}, "MountPoints": mounts}))
        self.write(f"{folder}/hostconfig.json", json.dumps({"RestartPolicy": {"Name": policy, "MaximumRetryCount": 0},
                                                            "Binds": None}))

    def policy(self, name):
        with open(f"{self.root}/docker/containers/{name}/hostconfig.json") as f:
            return json.load(f)

    def test_a_container_without_its_gate_wont_restart(self):
        self.container("old", "llama", gated=False)
        self.assertIsNone(self.run_it("hold"))
        host = self.policy("old")
        self.assertEqual(host["RestartPolicy"], {"Name": "no", "MaximumRetryCount": 0})
        # The rest is kept as Docker wrote it.
        self.assertIn("Binds", host)
        self.assertEqual(os.stat(f"{self.root}/docker/containers/old/hostconfig.json").st_mode & 0o777, 0o600)

    def test_one_with_its_gate_is_left(self):
        self.container("new", "llama", gated=True)
        self.assertIsNone(self.run_it("hold"))
        self.assertEqual(self.policy("new")["RestartPolicy"]["Name"], "on-failure")

    def test_anothers_gate_doesnt_count(self):
        self.container("odd", "comfy", gated=False)
        config = f"{self.root}/docker/containers/odd/config.v2.json"
        with open(config) as f:
            data = json.load(f)
        data["MountPoints"]["/g"] = {"Source": f"{self.root}/run/apps/llama"}
        self.write("docker/containers/odd/config.v2.json", json.dumps(data))
        self.assertIsNone(self.run_it("hold"))
        self.assertEqual(self.policy("odd")["RestartPolicy"]["Name"], "no")

    def test_only_apps_containers(self):
        self.container("mine", "", gated=False)
        self.container("odd-name", "../etc", gated=False)
        self.assertIsNone(self.run_it("hold"))
        self.assertEqual(self.policy("mine")["RestartPolicy"]["Name"], "on-failure")
        self.assertEqual(self.policy("odd-name")["RestartPolicy"]["Name"], "on-failure")

    def test_what_docker_couldnt_load_either_is_left(self):
        self.container("broken", "llama", gated=False)
        self.write("docker/containers/broken/config.v2.json", "{")
        self.write("docker/containers/empty/hostconfig.json", "")
        self.write("docker/containers/no-policy/config.v2.json", json.dumps({"Config": {"Labels": {}}, "MountPoints": {}}))
        self.assertIsNone(self.run_it("hold"))
        self.assertEqual(self.policy("broken")["RestartPolicy"]["Name"], "on-failure")

    def test_a_layout_it_doesnt_know_stops_docker(self):
        for config, host in (({"Config": {}, "MountPoints": {}}, {"RestartPolicy": {"Name": "always"}}),
                             ({"Config": {"Labels": {}}}, {"RestartPolicy": {"Name": "always"}}),
                             ({"Config": {"Labels": {}}, "MountPoints": {"/x": {}}}, {"RestartPolicy": {"Name": "always"}}),
                             ({"Config": {"Labels": {}}, "MountPoints": {}}, {"Restart": "always"})):
            self.write("docker/containers/new/config.v2.json", json.dumps(config))
            self.write("docker/containers/new/hostconfig.json", json.dumps(host))
            self.assertIn("doesn't know", str(self.run_it("hold")), (config, host))

    def test_a_policy_it_cant_write_stops_docker(self):
        self.container("old", "llama", gated=False)
        real = self.w.os.rename
        self.w.os.rename = lambda *a: (_ for _ in ()).throw(OSError(28, "No space left on device"))
        try:
            self.assertIn("couldn't be turned off", str(self.run_it("hold")))
        finally:
            self.w.os.rename = real

    def test_never_while_docker_runs(self):
        self.container("old", "llama", gated=False)
        self.write("docker-active", "")
        self.assertIsNotNone(self.run_it("hold"))
        self.assertEqual(self.policy("old")["RestartPolicy"]["Name"], "on-failure")

    def test_no_containers_yet(self):
        self.assertIsNone(self.run_it("hold"))


class Models(Base):
    OWN = {"name": "mine.gguf", "sha256": "a" * 64, "size": 5, "url": "https://example.com/mine.gguf"}

    def test_defaults_without_an_entry(self):
        self.assertEqual(self.w.library("llama", self.w.manifest("llama")), {"have": ["a.gguf"], "use": "a.gguf", "own": []})
        self.assertEqual(self.w.library("comfy", self.w.manifest("comfy")), {"have": ["sd.safetensors"], "use": "", "own": []})

    def test_use_keeps_it_and_starts_the_app_again(self):
        self.choose("platform")
        self.assertIsNone(self.run_it("model", "use", "llama", "b.gguf"))
        self.assertEqual(self.models()["llama"], {"have": ["a.gguf", "b.gguf"], "use": "b.gguf", "own": []})
        self.before("systemctl stop patai-workload", "systemctl start --no-block patai-workload")
        # Stopped before the choice was kept, so nothing already starting could take it.
        self.assertIn("models.json at the stop: \n", self.calls())
        self.choose("platform")
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("LLAMA_MODEL=b.gguf", self.calls())

    def test_use_of_an_app_not_running_doesnt_start_it(self):
        self.assertIsNone(self.run_it("model", "use", "llama", "b.gguf"))
        self.assertNotIn("systemctl", self.calls())

    def test_an_app_that_uses_every_model(self):
        self.assertIn("every model", str(self.run_it("model", "use", "comfy", "sd.safetensors")))

    def test_get_downloads(self):
        self.assertIsNone(self.run_it("model", "get", "llama", "b.gguf"))
        self.assertEqual(self.models()["llama"]["have"], ["a.gguf", "b.gguf"])
        self.assertIn("systemd-run --quiet --collect --unit=patai-fetch", self.calls())
        self.assertIn("patai-fetch llama", self.calls().replace(f"{self.root}/bin/", ""))

    def test_remove(self):
        self.write("models/llama/b.gguf", "weights")
        self.run_it("model", "get", "llama", "b.gguf")
        self.assertIn("choose another", str(self.run_it("model", "remove", "llama", "a.gguf")))
        self.assertIsNone(self.run_it("model", "remove", "llama", "b.gguf"))
        self.assertFalse(os.path.exists(f"{self.root}/models/llama/b.gguf"))
        self.assertEqual(self.models()["llama"]["have"], ["a.gguf"])

    def test_remove_takes_a_models_files_unless_another_kept_model_needs_them(self):
        projector = {"files": [{"name": "p.gguf", "sha256": "1" * 64, "size": 1, "url": "https://x/p"}]}
        self.app("llama", env={"LLAMA_CONTEXT": "8192"}, block={"env": "LLAMA_MODEL", "dir": "", "formats": ["gguf"]},
                 models=[("a.gguf", "", True), ("b.gguf", "", False, {}, projector), ("c.gguf", "", False, {}, projector)])
        for name in ("b.gguf", "c.gguf", "p.gguf"):
            self.write(f"models/llama/{name}", "weights")
        self.run_it("model", "get", "llama", "b.gguf")
        self.run_it("model", "get", "llama", "c.gguf")
        self.assertIsNone(self.run_it("model", "remove", "llama", "b.gguf"))
        self.assertTrue(os.path.exists(f"{self.root}/models/llama/p.gguf"))
        self.assertIsNone(self.run_it("model", "remove", "llama", "c.gguf"))
        self.assertEqual(os.listdir(f"{self.root}/models/llama"), [])
        self.assertIn("already", str(self.run_it("model", "add", "llama", "p.gguf", "https://e/x")))

    def test_remove_leaves_a_link_and_goes_nowhere_else(self):
        victim = self.write("victim/b.gguf", "precious")
        os.makedirs(f"{self.root}/models/llama")
        os.symlink(f"{self.root}/victim", f"{self.root}/models/comfy")
        self.write("models/llama/b.gguf", "weights")
        self.assertIn("isn't a plain directory", str(self.run_it("model", "remove", "comfy", "sd.safetensors")))
        self.assertTrue(os.path.exists(victim))
        self.assertFalse(os.path.exists(self.w.MODELS_STATE))

    def test_record_and_use_an_owners_model(self):
        own = self.OWN
        self.assertIsNone(self.run_it("model", "record", "llama", own["name"], own["sha256"], "5", own["url"]))
        self.assertEqual(self.models()["llama"]["own"], [own])
        self.assertIn("mine.gguf", self.models()["llama"]["have"])
        self.assertIsNone(self.run_it("model", "use", "llama", "mine.gguf"))
        self.assertIsNone(self.run_it("model", "use", "llama", "a.gguf"))
        self.assertIsNone(self.run_it("model", "remove", "llama", "mine.gguf"))
        self.assertEqual(self.models()["llama"]["own"], [])

    def test_a_bad_record(self):
        for sha256, size, url in (("x" * 64, "5", "-"), ("a" * 64, "0", "-"), ("a" * 64, "5", "ftp://x/y")):
            self.assertIsNotNone(self.run_it("model", "record", "llama", "mine.gguf", sha256, size, url))
        self.assertFalse(os.path.exists(self.w.MODELS_STATE))

    def test_add(self):
        self.assertIsNone(self.run_it("model", "add", "llama", "mine.gguf", "https://example.com/mine.gguf"))
        self.assertIn("--add llama mine.gguf https://example.com/mine.gguf", self.calls())
        self.assertIsNone(self.run_it("model", "add-upload", "comfy", "mine.safetensors"))
        self.assertIn("--add-upload comfy mine.safetensors", self.calls())

    def test_add_refuses(self):
        for app, name, url in (("llama", "mine.ckpt", "https://e/x"), ("llama", "mine.safetensors", "https://e/x"),
                               ("llama", "a.gguf", "https://e/x"), ("llama", "../x.gguf", "https://e/x"),
                               ("llama", "mine.gguf", "file:///etc/passwd"), ("llama", "mine.gguf", "https://u:p@e/x"),
                               ("yolo", "mine.gguf", "https://e/x")):
            self.assertIsNotNone(self.run_it("model", "add", app, name, url), (app, name, url))
        self.assertNotIn("systemd-run", self.calls())

    def test_a_record_it_cant_read_drops_out(self):
        self.write("provision/models.json", json.dumps({"llama": {"have": ["a.gguf", "x.gguf", "mine.gguf"], "use": "x.gguf",
                                                                  "own": [{"name": "../x", "sha256": "a" * 64, "size": 1}, self.OWN]}}))
        lib = self.w.library("llama", self.w.manifest("llama"))
        self.assertEqual(lib, {"have": ["a.gguf", "mine.gguf"], "use": "a.gguf", "own": [self.OWN]})

    def test_remove_waits_for_no_download(self):
        import fcntl
        self.write("models/llama/b.gguf", "weights")
        self.run_it("model", "get", "llama", "b.gguf")
        with open(self.w.FETCH_LOCK, "w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            self.assertIn("download is running", str(self.run_it("model", "remove", "llama", "b.gguf")))
        self.assertTrue(os.path.exists(f"{self.root}/models/llama/b.gguf"))
        self.assertIn("b.gguf", self.models()["llama"]["have"])
        self.assertIsNone(self.run_it("model", "remove", "llama", "b.gguf"))

    def test_serve_wont_start_a_model_fetch_didnt_check(self):
        self.choose("platform")
        self.write("fetch-changes", json.dumps({"llama": {"have": ["a.gguf", "b.gguf"], "use": "b.gguf", "own": []}}))
        self.assertIsNone(self.run_it("serve"))
        self.assertNotIn(" up ", self.calls())

    def test_show_has_each_apps_models(self):
        self.run_it("show")
        self.assertEqual(json.loads(self.out)["models"]["llama"]["use"], "a.gguf")


class Choosing(Base):
    def test_start_keeps_what_runs_and_adds_the_app(self):
        self.choose("studio")
        self.write("projects", "comfy\n")
        self.assertIsNone(self.run_it("start", "platform"))
        self.before("systemctl stop patai-workload", "systemctl start --no-block patai-workload")
        self.assertNotIn("down", self.calls())
        self.assertEqual(self.state()["chosen"], ["studio", "platform"])

    def test_start_of_something_that_isnt_an_app(self):
        for name in ("llama", "yolo", "nothing", "../etc"):
            self.assertIsNotNone(self.run_it("start", name))
        self.assertNotIn("systemctl", self.calls())

    def test_stop(self):
        self.choose("platform")
        os.makedirs(f"{self.w.GATES}/llama")
        self.write("projects", "llama\n")
        self.assertIsNone(self.run_it("stop", "platform"))
        self.before("systemctl stop patai-workload", "-p llama down --remove-orphans")
        self.assertNotIn("systemctl start", self.calls())
        self.assertEqual(self.state(), {"chosen": [], "apis": {}, "settings": {}})
        self.assertFalse(self.gate("llama"))

    def test_stopping_one_not_on_leaves_the_rest_getting_ready(self):
        self.choose("platform")
        self.write("projects", "comfy\nllama\n")
        self.assertIsNone(self.run_it("stop", "studio"))
        self.assertNotIn("systemctl", self.calls())
        self.assertIn("-p comfy down --remove-orphans", self.calls())
        self.assertNotIn("-p llama down", self.calls())

    def test_stopping_one_of_several_serves_the_rest(self):
        self.choose("platform", "studio")
        self.write("projects", "comfy\nllama\n")
        self.assertIsNone(self.run_it("stop", "platform"))
        self.before("systemctl stop patai-workload", "systemctl start --no-block patai-workload")
        self.assertIn("-p llama down", self.calls())
        self.assertNotIn("-p comfy down", self.calls())
        self.assertEqual(self.state()["chosen"], ["studio"])

    def test_a_shared_service_stays_for_the_other_app(self):
        self.choose("platform", "studio", images=True)
        self.write("projects", "comfy\nllama\n")
        self.assertIsNone(self.run_it("stop", "studio"))
        self.assertNotIn("down", self.calls())

    def test_stop_of_something_that_isnt_an_app(self):
        for name in ("llama", "nothing", "../etc"):
            self.assertIsNotNone(self.run_it("stop", name))
        self.assertNotIn("systemctl", self.calls())

    def test_an_api_on_starts_its_service(self):
        self.choose("platform")
        self.assertIsNone(self.run_it("api", "on", "platform", "images"))
        self.assertEqual(self.state()["apis"], {"platform": ["chat", "images"]})
        self.before("systemctl stop patai-workload", "systemctl start --no-block patai-workload")
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("-p comfy start server", self.calls())

    def test_an_api_off_takes_its_service_down(self):
        self.choose("platform", images=True)
        self.write("projects", "comfy\nllama\n")
        self.assertIsNone(self.run_it("api", "off", "platform", "images"))
        self.assertIn("-p comfy down --remove-orphans", self.calls())
        self.assertNotIn("-p llama down", self.calls())
        self.assertEqual(self.state()["apis"], {"platform": ["chat"]})

    def test_an_apps_apis_are_kept_while_its_off(self):
        self.assertIsNone(self.run_it("api", "on", "platform", "images"))
        self.assertNotIn("systemctl start", self.calls())
        self.assertEqual(self.state()["chosen"], [])
        self.assertIsNone(self.run_it("start", "platform"))
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("-p comfy start server", self.calls())

    def test_an_api_refused(self):
        self.assertIn("always on", str(self.run_it("api", "off", "platform", "chat")))
        for args in (("on", "platform", "nothing"), ("on", "nothing", "chat"), ("on", "studio", "comfy"), ("up", "platform", "chat")):
            self.assertIsNotNone(self.run_it("api", *args), args)
        self.assertNotIn("systemctl", self.calls())

    def test_fetch_gets_an_apps_services_in_a_service_of_its_own(self):
        self.assertIsNone(self.run_it("fetch", "studio"))
        self.assertIn("systemd-run --quiet --collect --unit=patai-fetch", self.calls())
        self.assertIn("patai-fetch comfy", self.calls().replace(f"{self.root}/bin/", ""))
        self.assertIsNone(self.run_it("api", "on", "platform", "images"))
        self.assertIsNone(self.run_it("fetch", "platform"))
        self.assertIn("patai-fetch llama comfy", self.calls().replace(f"{self.root}/bin/", ""))
        self.assertIsNotNone(self.run_it("fetch", "llama"))

    def test_show(self):
        self.choose("platform")
        self.assertIsNone(self.run_it("show"))
        shown = json.loads(self.out)
        self.assertEqual((shown["chosen"], shown["apis"], shown["services"]),
                         (["platform"], {"platform": ["chat"], "studio": []}, ["llama"]))
        self.assertEqual(sorted(shown["models"]), ["comfy", "llama"])

    def test_usage(self):
        for args in ((), ("serve", "x"), ("start",), ("bogus", "llama"), ("api", "on", "platform")):
            self.assertIsNotNone(self.run_it(*args))


class Settings(Base):
    SETTINGS = [
        {"name": "context", "title": "Context", "type": "int", "default": 8192, "min": 1024, "max": 65536, "env": "LLAMA_CONTEXT"},
        {"name": "temperature", "title": "Temperature", "type": "number", "default": 0.8, "min": 0, "max": 2, "env": "TEMP"},
        {"name": "cache_8bit", "title": "8-bit cache", "type": "bool", "default": False,
         "envs": {"true": {"CACHE": "q8_0"}, "false": {}}},
        {"name": "effort", "title": "Effort", "type": "choice", "choices": ["low", "high"], "default": "low", "env": "EFFORT"},
        {"name": "waiting", "title": "Waiting", "type": "int", "default": 2, "min": 0, "max": 8, "for": "gateway"},
    ]

    def setUp(self):
        super().setUp()
        self.app("llama", models=[("a.gguf", "", True, {}, {"settings": {"temperature": 1.0, "cache_8bit": True},
                                                            "limits": {"context": 32768}}),
                                  ("b.gguf", "", False)],
                 block={"env": "LLAMA_MODEL", "dir": "", "formats": ["gguf"]}, settings=self.SETTINGS)
        self.choose("platform")

    def shown(self):
        self.assertIsNone(self.run_it("show"))
        return json.loads(self.out)["settings"]["llama"]

    def test_defaults_are_the_models_then_the_manifests(self):
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("LLAMA_CONTEXT=8192 LLAMA_MODEL=a.gguf TEMP=1 CACHE=q8_0", self.calls())
        shown = self.shown()
        self.assertEqual((shown["model"], shown["values"]["temperature"], shown["changed"], shown["limits"]),
                         ("a.gguf", 1.0, [], {"context": 32768}))

    def test_a_change_to_the_container_restarts_it(self):
        self.assertIsNone(self.run_it("set", "llama", '{"context": 16384, "temperature": 0.5}'))
        self.before("systemctl stop patai-workload", "systemctl start --no-block patai-workload")
        self.assertEqual(self.state()["settings"], {"llama": {"a.gguf": {"context": 16384, "temperature": 0.5}}})
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("LLAMA_CONTEXT=16384 LLAMA_MODEL=a.gguf TEMP=0.5 CACHE=q8_0", self.calls())
        self.assertEqual(self.shown()["changed"], ["context", "temperature"])

    def test_a_change_for_the_gateway_restarts_nothing(self):
        self.assertIsNone(self.run_it("set", "llama", '{"waiting": 4}'))
        self.assertNotIn("systemctl", self.calls())
        self.assertEqual(self.shown()["values"]["waiting"], 4)

    def test_null_puts_the_default_back(self):
        self.assertIsNone(self.run_it("set", "llama", '{"context": 16384, "waiting": 4}'))
        self.assertIsNone(self.run_it("set", "llama", '{"context": null}'))
        self.assertEqual(self.state()["settings"], {"llama": {"a.gguf": {"waiting": 4}}})
        self.assertIsNone(self.run_it("set", "llama", '{"waiting": null}'))
        self.assertEqual(self.state()["settings"], {})

    def test_each_model_keeps_its_own(self):
        self.assertIsNone(self.run_it("set", "llama", '{"context": 16384}'))
        self.assertIsNone(self.run_it("model", "use", "llama", "b.gguf"))
        shown = self.shown()
        self.assertEqual((shown["model"], shown["values"]["context"], shown["values"]["temperature"], shown["changed"]),
                         ("b.gguf", 8192, 0.8, []))
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("LLAMA_CONTEXT=8192 LLAMA_MODEL=b.gguf TEMP=0.8 CACHE= KEY", self.calls())

    def test_refused(self):
        for asked in ('{"context": 65536}', '{"context": 512}', '{"context": "big"}', '{"context": true}',
                      '{"temperature": "hot"}', '{"cache_8bit": 1}', '{"effort": "max"}', '{"nothing": 1}', '{}',
                      '[1]', 'nope'):
            self.assertIsNotNone(self.run_it("set", "llama", asked), asked)
        for args in (("set", "comfy", '{"x": 1}'), ("set", "nothing", '{"x": 1}'), ("set", "llama", "x" * 5000)):
            self.assertIsNotNone(self.run_it(*args), args)
        self.assertNotIn("systemctl", self.calls())

    def test_a_kept_value_that_no_longer_fits_has_the_default(self):
        self.w.choose({"chosen": ["platform"], "apis": {}, "settings": {"llama": {"a.gguf": {"context": 999999, "effort": "high"}}}})
        shown = self.shown()
        self.assertEqual((shown["values"]["context"], shown["values"]["effort"], shown["changed"]), (8192, "high", ["effort"]))


class Admission(Base):
    def setUp(self):
        super().setUp()
        context = {"name": "context", "title": "Context", "type": "int", "default": 8192, "min": 1024, "max": 131072,
                   "env": "LLAMA_CONTEXT"}
        self.app("llama", models=[("a.gguf", "", True, {}, {"memory": {"base_mib": 12000, "kv_mib_per_1k": 100}}),
                                  ("big.gguf", "", False, {}, {"memory": {"base_mib": 20000, "kv_mib_per_1k": 100}})],
                 block={"env": "LLAMA_MODEL", "dir": "", "formats": ["gguf"]}, settings=[context])
        size = {"name": "size", "title": "Largest size", "type": "int", "choices": [512, 1024], "default": 512, "for": "gateway"}
        self.app("comfy", models=[("sd.safetensors", "checkpoints", True)], block={"dir": "checkpoints", "formats": []},
                 settings=[size])
        found = json.loads(open(f"{self.root}/recipes/comfy/manifest.json").read())
        found["memory"] = {"by": "size", "values": {"512": 10000, "1024": 17000}}
        self.write("recipes/comfy/manifest.json", json.dumps(found))
        self.choose("platform")

    def test_what_fits_is_taken(self):
        # 12000 + 800 for 8k of context, and 10000 for images at 512.
        self.assertIsNone(self.run_it("api", "on", "platform", "images"))
        self.assertIsNone(self.run_it("show"))
        self.assertEqual(json.loads(self.out)["memory"], {"budget": 26624, "services": {"llama": 12800, "comfy": 10000}})

    def test_a_larger_size_that_doesnt_fit_is_refused(self):
        self.assertIsNone(self.run_it("api", "on", "platform", "images"))
        refused = str(self.run_it("set", "comfy", '{"size": 1024}'))
        self.assertIn("needs 29.1 GiB, past the 26 GiB for apps: chat 12.5, images 16.6", refused)
        self.assertEqual(self.state()["settings"], {})

    def test_more_context_that_doesnt_fit_is_refused(self):
        self.assertIsNone(self.run_it("api", "on", "platform", "images"))
        self.assertIsNotNone(self.run_it("set", "llama", '{"context": 65536}'))
        self.assertIsNone(self.run_it("set", "llama", '{"context": 16384}'))

    def test_a_model_that_doesnt_fit_is_refused(self):
        self.assertIsNone(self.run_it("api", "on", "platform", "images"))
        os.remove(f"{self.root}/calls")
        self.assertIn("past the 26 GiB", str(self.run_it("model", "use", "llama", "big.gguf")))
        self.assertNotIn("systemctl stop", self.calls())
        self.assertIsNone(self.run_it("api", "off", "platform", "images"))
        self.assertIsNone(self.run_it("model", "use", "llama", "big.gguf"))

    def test_turning_images_on_past_it_is_refused(self):
        self.assertIsNone(self.run_it("model", "use", "llama", "big.gguf"))
        self.assertIn("past the 26 GiB", str(self.run_it("api", "on", "platform", "images")))

    def test_less_is_always_taken(self):
        self.w.choose({"chosen": ["platform"], "apis": {"platform": ["chat", "images"]},
                       "settings": {"comfy": {"": {"size": 1024}}}})
        self.assertIsNone(self.run_it("set", "comfy", '{"size": 512}'))

    def test_each_request_at_once_keeps_its_checkpoints(self):
        found = json.loads(open(f"{self.root}/recipes/llama/manifest.json").read())
        found["models"][0]["memory"]["request_mib"] = 1200
        found["settings"].append({"name": "parallel", "title": "Requests at once", "type": "int", "default": 2, "min": 1,
                                  "max": 4, "env": "LLAMA_ARG_N_PARALLEL"})
        self.write("recipes/llama/manifest.json", json.dumps(found))
        # 12000, 800 for 8k of context, and 1200 for each of 2 requests.
        self.assertEqual(self.w.needs(self.w.desired())["llama"], 12000 + 800 + 2 * 1200)
        self.assertIsNone(self.run_it("set", "llama", '{"parallel": 4}'))
        self.assertEqual(self.w.needs(self.w.desired())["llama"], 12000 + 800 + 4 * 1200)

    def test_an_owners_model_is_estimated_from_its_file(self):
        self.write("provision/models.json", json.dumps({"llama": {"have": ["a.gguf", "mine.gguf"], "use": "mine.gguf", "own": [
            {"name": "mine.gguf", "sha256": "c" * 64, "size": 8 * 2**30, "url": ""}]}}))
        self.assertEqual(self.w.needs(self.w.desired())["llama"], 8192 + 1024 + 96 * 8)


class Secrets(Base):
    def setUp(self):
        super().setUp()
        found = json.loads(open(f"{self.root}/recipes/llama/manifest.json").read())
        found["secrets"] = [{"name": "key", "title": "A key", "env": "KEY"}, {"name": "made", "title": "Its own", "env": "MADE", "made": True}]
        # Its own user, which can't be under 1000: run as root, any will do.
        self.uid = os.getuid() if os.getuid() >= 1000 else 1000
        found["data"] = {"uid": self.uid}
        found["networks"] = ["patai-ai"]
        self.write("recipes/llama/manifest.json", json.dumps(found))
        self.choose("platform")

    def secret(self, value):
        stdin = sys.stdin
        sys.stdin = io.StringIO(value)
        try:
            return self.run_it("secret", "llama", "key")
        finally:
            sys.stdin = stdin

    def test_a_secret_reaches_the_container_and_is_never_shown(self):
        self.assertIsNone(self.secret("abc+/=123\n"))
        self.before("systemctl stop patai-workload", "systemctl start --no-block patai-workload")
        self.assertEqual(os.stat(self.w.SECRETS).st_mode & 0o777, 0o600)
        self.assertIsNone(self.run_it("serve"))
        self.assertIn("KEY=abc+/=123 MADE=", self.calls())
        self.assertIsNone(self.run_it("show"))
        self.assertEqual(json.loads(self.out)["secrets"], {"llama": {"key": True, "made": True}})
        self.assertNotIn("abc", self.out)

    def test_one_the_service_makes_is_made_once(self):
        self.assertIsNone(self.run_it("serve"))
        made = json.load(open(self.w.SECRETS))["llama"]["made"]
        self.assertGreater(len(made), 30)
        self.assertIsNone(self.run_it("serve"))
        self.assertEqual(json.load(open(self.w.SECRETS))["llama"]["made"], made)
        self.assertIsNotNone(self.run_it("secret", "llama", "made"))

    def test_empty_removes_it(self):
        self.assertIsNone(self.secret("abc"))
        self.assertIsNone(self.secret(""))
        self.assertEqual(json.load(open(self.w.SECRETS))["llama"], {})

    def test_refused(self):
        self.assertIsNotNone(self.secret("a\x7fcontrol"))
        self.assertIsNotNone(self.secret("x" * 600))
        self.assertIsNotNone(self.run_it("secret", "llama", "nothing"))
        self.assertIsNotNone(self.run_it("secret", "comfy", "key"))
        self.assertFalse(os.path.exists(self.w.SECRETS))

    def test_its_data_folder_is_made_for_it(self):
        self.assertIsNone(self.run_it("serve"))
        info = os.stat(f"{self.root}/data/llama")
        self.assertEqual((info.st_mode & 0o777, info.st_uid), (0o700, self.uid))

    def test_a_link_in_place_of_its_data_folder_is_refused(self):
        os.makedirs(f"{self.root}/elsewhere")
        os.makedirs(f"{self.root}/data")
        os.symlink(f"{self.root}/elsewhere", f"{self.root}/data/llama")
        self.assertIsNotNone(self.run_it("serve"))
        self.assertNotIn(" up ", self.calls())
        self.assertIn("isn't a plain directory", self.calls())

    def test_the_network_it_shares_is_made_first(self):
        self.assertIsNone(self.run_it("serve"))
        self.before("docker network inspect patai-ai", "up --no-start")


class AppKeys(Base):
    def setUp(self):
        super().setUp()
        self.write("apps/chat/manifest.json", json.dumps({"name": "chat", "apis": [
            {"name": "chat", "service": "llama", "always": True}, {"name": "search", "service": "search"}],
            "services": ["comfy"], "key": "GW", "secrets": [{"name": "db", "env": "DB"}]}))
        self.app("search")
        found = json.loads(open(f"{self.root}/recipes/comfy/manifest.json").read())
        found["secrets"] = [{"name": "pw", "title": "A password", "env": "ONCE", "once": True}]
        self.write("recipes/comfy/manifest.json", json.dumps(found))
        self.w.choose({"chosen": ["chat"], "apis": {}, "settings": {}})

    def composed(self, app):
        return [line for line in self.calls().splitlines() if f" -p {app} up " in line]

    def give(self, value):
        stdin = sys.stdin
        sys.stdin = io.StringIO(value)
        try:
            return self.run_it("secret", "comfy", "pw")
        finally:
            sys.stdin = stdin

    def test_an_apps_own_services_get_its_key_secrets_and_apis(self):
        self.assertIsNone(self.run_it("serve"))
        kept = json.load(open(self.w.SECRETS))["app:chat"]
        own, = self.composed("comfy")
        self.assertIn(f"GW={kept['+key']} DB={kept['db']} APIS=chat", own)
        # A service behind one of its APIs is anyone's, and gets none of it.
        self.assertIn("GW= DB= APIS=", self.composed("llama")[0])
        self.assertIsNone(self.run_it("show"))
        shown = json.loads(self.out)
        self.assertEqual(shown["keys"], {"chat": hashlib.sha256(kept["+key"].encode()).hexdigest()})
        self.assertNotIn(kept["+key"], self.out)

    def test_once_is_given_until_it_answers(self):
        self.assertIsNone(self.give("first pass"))
        self.w.answers = lambda app, found: True
        self.assertIsNone(self.run_it("serve"))
        first, again = self.composed("comfy")
        self.assertIn("ONCE=first pass ", first)
        self.assertIn("ONCE= ", again)
        self.assertEqual(json.load(open(self.w.SECRETS))["comfy"], {"+used": ["pw"]})
        self.assertIsNone(self.run_it("show"))
        self.assertEqual((json.loads(self.out)["secrets"], json.loads(self.out)["used"]),
                         ({"comfy": {"pw": False}}, {"comfy": ["pw"]}))
        # Given again, it's for the next start again.
        self.assertIsNone(self.give("second"))
        self.assertEqual(json.load(open(self.w.SECRETS))["comfy"], {"pw": "second", "+used": []})

    def test_once_is_kept_if_it_doesnt_answer(self):
        self.assertIsNone(self.give("first pass"))
        self.w.answers = lambda app, found: False
        self.w.ONCE_WAIT = 0
        self.assertIsNone(self.run_it("serve"))
        self.assertEqual(len(self.composed("comfy")), 1)
        self.assertEqual(json.load(open(self.w.SECRETS))["comfy"], {"pw": "first pass"})


class With(Base):
    def test_an_api_runs_the_services_it_brings(self):
        self.app("search")
        self.write("apps/platform/manifest.json", json.dumps({"name": "platform", "apis": [
            {"name": "chat", "service": "llama", "always": True},
            {"name": "agentic", "service": "comfy", "with": ["search", "../x"], "default": True}]}))
        self.choose("platform")
        self.assertEqual(self.w.wanted(self.w.desired()), ["llama", "comfy", "search"])


if __name__ == "__main__":
    unittest.main(argv=["patai-workload"], verbosity=1)
