from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
import json
from pathlib import Path

from cover_replay.acceptance import run_acceptance
from cover_replay.generator import generate_samples
from cover_replay.util import load_json, tree_digest


class GeneratorTests(unittest.TestCase):
    def test_seeded_generation_is_reproducible(self) -> None:
        with tempfile.TemporaryDirectory() as first, tempfile.TemporaryDirectory() as second:
            first_manifest = generate_samples(
                first, seed=99117, profiles=("small", "conflict")
            )
            second_manifest = generate_samples(
                second, seed=99117, profiles=("small", "conflict")
            )
            self.assertEqual(first_manifest, second_manifest)
            self.assertEqual(tree_digest(first), tree_digest(second))

    def test_conflict_sample_and_trajectory_fields(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            generate_samples(
                temporary,
                seed=99117,
                profiles=("small", "conflict"),
            )
            root = Path(temporary)
            expected = load_json(root / "conflict" / "expected.json")
            self.assertEqual(expected["expected_error"], "E_CREDIT_CONFLICT")
            self.assertEqual(
                expected["conflict"]["code"], "E_CREDIT_CONFLICT"
            )
            self.assertFalse((root / "conflict" / "baseline.json").exists())
            lines = (
                (root / "small" / "trajectory.jsonl")
                .read_text(encoding="utf-8")
                .splitlines()
            )
            first = json.loads(lines[0])
            self.assertIn("effective_client_sequences", first)
            self.assertIn("state_hash", first)
            self.assertIn("queue_watermark", first)
            self.assertGreater(len(lines), 0)

    def test_cli_validate(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            generate_samples(
                temporary, seed=7, profiles=("small",)
            )
            sample = Path(temporary) / "small"
            result = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "cover_replay",
                    "validate",
                    "--record",
                    str(sample / "match.json"),
                    "--config",
                    str(sample / "config.json"),
                ],
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('"ok":true', result.stdout)

    def test_full_acceptance_suite(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            report = run_acceptance(temporary, seed=424242)
            self.assertTrue(report["ok"], report)
            self.assertTrue(all(item["ok"] for item in report["checks"].values()))


if __name__ == "__main__":
    unittest.main()
