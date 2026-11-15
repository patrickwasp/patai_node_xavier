#!/usr/bin/env python3
"""Tests for overlay/usr/local/sbin/patai-report's ready checks. It has to pass on a unit's Python 3.8:

    python3 tests/patai-report.py
"""
import glob
import importlib.machinery
import importlib.util
import json
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "..", "overlay/usr/local/sbin/patai-report")


def load_script():
    loader = importlib.machinery.SourceFileLoader("patai_report", SCRIPT)
    spec = importlib.util.spec_from_loader("patai_report", loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class Answers(unittest.TestCase):
    def setUp(self):
        self.report = load_script()

    def test_a_health_check_answers_when_every_container_is_healthy(self):
        self.assertTrue(self.report.answers("healthcheck", "Up 2 minutes (healthy)\nUp 3 minutes (healthy)"))
        self.assertFalse(self.report.answers("healthcheck", "Up 2 minutes (healthy)\nUp 5 seconds (health: starting)"))
        self.assertFalse(self.report.answers("healthcheck", "Up 2 minutes (unhealthy)"))

    def test_anything_else_is_not_answering_and_never_fails_the_report(self):
        self.assertFalse(self.report.answers("", "Up 2 minutes"))
        self.assertFalse(self.report.answers("tcp", "Up 2 minutes"))

    def test_every_recipe_has_a_ready_check_it_reads(self):
        # A recipe whose check it didn't know stopped every report after the Chat app came in.
        for path in glob.glob(os.path.join(HERE, "..", "workloads", "*", "manifest.json")):
            with open(path) as f:
                ready = json.load(f).get("ready", "")
            with self.subTest(path=os.path.relpath(path, os.path.join(HERE, ".."))):
                self.assertTrue(ready.startswith("http://127.0.0.1:") or ready == "healthcheck", ready)


if __name__ == "__main__":
    unittest.main()
