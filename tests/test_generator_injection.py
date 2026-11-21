from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from tactical_pick.errors import E_SCENE_INVALID, INJECTION_CODES, PickError
from tactical_pick.generator import build_dataset, verify_dataset, write_dataset


ROOT = Path(__file__).resolve().parents[1]


class GeneratorTests(unittest.TestCase):
    def test_same_seed_is_byte_identical(self) -> None:
        first = build_dataset(7)
        second = build_dataset(7)
        self.assertEqual(first.sample_files, second.sample_files)
        self.assertEqual(first.baseline_files, second.baseline_files)
        self.assertEqual(
            [run.trace_bytes for run in first.runs],
            [run.trace_bytes for run in second.runs],
        )

    def test_different_seed_changes_inputs_and_baselines(self) -> None:
        first = build_dataset(7)
        second = build_dataset(8)
        self.assertNotEqual(
            first.sample_files["battlefield-8x8-seed-7.scenario.json"],
            second.sample_files["battlefield-8x8-seed-8.scenario.json"],
        )
        self.assertNotEqual(
            first.baseline_files["battlefield-8x8-seed-7.trace.jsonl"],
            second.baseline_files["battlefield-8x8-seed-8.trace.jsonl"],
        )

    def test_write_and_verify_dataset(self) -> None:
        bundle = build_dataset(7)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            samples = root / "samples"
            baselines = root / "baselines"
            write_dataset(bundle, samples, baselines)
            report = verify_dataset(samples, baselines)
            self.assertTrue(report["ok"])
            self.assertEqual(report["samples_verified"], 3)
            self.assertEqual(
                (samples / "manifest.json").read_bytes(),
                bundle.sample_files["manifest.json"],
            )
            self.assertEqual(
                (baselines / "manifest.json").read_bytes(),
                bundle.baseline_files["manifest.json"],
            )

    def test_verify_rejects_tampered_baseline_manifest(self) -> None:
        bundle = build_dataset(7)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            samples = root / "samples"
            baselines = root / "baselines"
            write_dataset(bundle, samples, baselines)
            manifest = json.loads((baselines / "manifest.json").read_text("utf-8"))
            manifest["samples"][0]["trace_bytes_hash"] = "0" * 64
            (baselines / "manifest.json").write_text(
                json.dumps(manifest, sort_keys=True, separators=(",", ":")),
                encoding="utf-8",
            )
            with self.assertRaises(PickError) as caught:
                verify_dataset(samples, baselines)
            self.assertEqual(caught.exception.code, E_SCENE_INVALID)
            self.assertIn("manifest", caught.exception.location["field"])


class InjectionCliTests(unittest.TestCase):
    def test_every_injection_has_an_independent_code_and_no_output(self) -> None:
        expected = {
            "degenerate-bounds": INJECTION_CODES["degenerate-bounds"],
            "zero-ray": INJECTION_CODES["zero-ray"],
            "out-of-range-screen": INJECTION_CODES["out-of-range-screen"],
            "self-intersect-region": INJECTION_CODES["self-intersect-region"],
            "corrupt-snapshot": INJECTION_CODES["corrupt-snapshot"],
        }
        for kind, code in expected.items():
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as temporary:
                environment = dict(os.environ)
                environment["PYTHONPATH"] = str(ROOT)
                completed = subprocess.run(
                    [
                        sys.executable,
                        "-m",
                        "tactical_pick",
                        "inject",
                        "--kind",
                        kind,
                    ],
                    cwd=temporary,
                    env=environment,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(completed.returncode, 2, completed.stderr)
                payload = json.loads(completed.stderr)
                self.assertEqual(payload["code"], code)
                self.assertIn("injection_point", payload["location"])
                self.assertIn("cause_code", payload)
                self.assertEqual(list(Path(temporary).iterdir()), [])

    def test_generate_and_verify_cli(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            samples = root / "samples"
            baselines = root / "baselines"
            generate = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "tactical_pick",
                    "generate",
                    "--seed",
                    "11",
                    "--samples",
                    str(samples),
                    "--baselines",
                    str(baselines),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(generate.returncode, 0, generate.stderr)
            verified = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "tactical_pick",
                    "verify",
                    "--samples",
                    str(samples),
                    "--baselines",
                    str(baselines),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(verified.returncode, 0, verified.stderr)
            report = json.loads(verified.stdout)
            self.assertTrue(report["ok"])
            self.assertEqual(report["samples_verified"], 3)

    def test_generate_inject_flag_writes_nothing(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            samples = root / "samples"
            baselines = root / "baselines"
            completed = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "tactical_pick",
                    "generate",
                    "--inject",
                    "zero-ray",
                    "--samples",
                    str(samples),
                    "--baselines",
                    str(baselines),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 2, completed.stderr)
            payload = json.loads(completed.stderr)
            self.assertEqual(payload["code"], INJECTION_CODES["zero-ray"])
            self.assertFalse(samples.exists())
            self.assertFalse(baselines.exists())


if __name__ == "__main__":
    unittest.main()
