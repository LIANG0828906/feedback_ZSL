"""Shared test harness: generate deterministic samples into a temp dir."""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from scripts.generate_samples import main as generate_main


class SampleBackedTestCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp_dir = tempfile.TemporaryDirectory(prefix="scorebench-tests-")
        cls.sample_root = Path(cls.temp_dir.name) / "samples"
        import sys
        old_argv = __import__("sys").argv
        sys.argv = [
            "generate_samples", "--output", str(cls.sample_root)
        ]
        try:
            generate_main()
        finally:
            sys.argv = old_argv
        cls.v1 = cls.sample_root / "score_v1.json"
        cls.v2 = cls.sample_root / "score_v2.json"
        cls.v3 = cls.sample_root / "score_v3.json"
        cls.broken = cls.sample_root / "broken"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp_dir.cleanup()
