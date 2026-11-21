"""Shared test fixtures: deterministic sample suite in a temp workspace."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
import unittest

from syncbench.generator import standard_suite


class SampleSuiteTestCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="syncbench_test_")
        cls.data_dir = os.path.join(cls.tmp, "data")
        cls.outputs = standard_suite(cls.data_dir)
        cls.out_root = os.path.join(cls.tmp, "out")
        os.makedirs(cls.out_root, exist_ok=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def dataset_dir(self, name):
        return os.path.join(self.data_dir, name)

    def out_dir(self, name):
        path = os.path.join(self.out_root, name)
        os.makedirs(path, exist_ok=True)
        return path

    def load_truth(self, name):
        with open(os.path.join(self.dataset_dir(name), "truth.json")) as fh:
            return json.load(fh)
