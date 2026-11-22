from __future__ import annotations

import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from tests.helpers import ROOT, SAMPLES


class DeterministicSampleTests(unittest.TestCase):
    def test_committed_samples_match_manifest(self) -> None:
        manifest = json.loads(
            (SAMPLES / "generator_manifest.json").read_text(encoding="utf-8")
        )
        for relative, expected in manifest["files"].items():
            actual = hashlib.sha256((SAMPLES / relative).read_bytes()).hexdigest()
            self.assertEqual(expected, actual, relative)

    def test_generator_is_repeatable(self) -> None:
        with tempfile.TemporaryDirectory() as first, tempfile.TemporaryDirectory() as second:
            command = [
                sys.executable,
                str(ROOT / "scripts" / "generate_samples.py"),
                "--seed",
                "20261123",
            ]
            subprocess.run(
                command + ["--output", first],
                check=True,
                cwd=ROOT,
                capture_output=True,
                text=True,
            )
            subprocess.run(
                command + ["--output", second],
                check=True,
                cwd=ROOT,
                capture_output=True,
                text=True,
            )
            first_path = Path(first)
            second_path = Path(second)
            names = sorted(
                path.relative_to(first_path)
                for path in first_path.rglob("*")
                if path.is_file()
            )
            self.assertEqual(
                names,
                sorted(
                    path.relative_to(second_path)
                    for path in second_path.rglob("*")
                    if path.is_file()
                ),
            )
            for relative in names:
                self.assertEqual(
                    (first_path / relative).read_bytes(),
                    (second_path / relative).read_bytes(),
                    str(relative),
                )
                self.assertEqual(
                    (first_path / relative).read_bytes(),
                    (SAMPLES / relative).read_bytes(),
                    str(relative),
                )


if __name__ == "__main__":
    unittest.main()
