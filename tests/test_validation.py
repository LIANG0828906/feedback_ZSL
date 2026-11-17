from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from drum_bus.errors import ValidationFailure
from drum_bus.manifest import load_project
from drum_bus.synth import DemoConfig, generate_demo_project


ROOT = Path(__file__).resolve().parents[1]


def _config() -> DemoConfig:
    return DemoConfig(
        duration_seconds=1.0,
        reference_times=(0.2, 0.5, 0.8),
        block_size=512,
    )


def _load_manifest(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def _write_manifest(path: Path, value: dict) -> None:
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


class ValidationTests(unittest.TestCase):
    def _case(self, directory: str) -> Path:
        return generate_demo_project(directory, _config())

    def _codes(self, failure: ValidationFailure) -> set[str]:
        return {issue.code for issue in failure.issues}

    def test_missing_file_has_channel_and_code(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            manifest = self._case(directory)
            (Path(directory) / "bangzi.wav").unlink()
            with self.assertRaises(ValidationFailure) as caught:
                load_project(manifest)
            issues = [issue for issue in caught.exception.issues if issue.code == "FILE_MISSING"]
            self.assertTrue(issues)
            self.assertIn("bangzi", {issue.channel for issue in issues})

    def test_sample_rate_and_bit_depth_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            manifest = self._case(directory)
            value = _load_manifest(manifest)
            value["sample_rate"] = 44100
            value["bit_depth"] = 16
            _write_manifest(manifest, value)
            with self.assertRaises(ValidationFailure) as caught:
                load_project(manifest)
            codes = self._codes(caught.exception)
            self.assertIn("SAMPLE_RATE_MISMATCH", codes)
            self.assertIn("BIT_DEPTH_MISMATCH", codes)
            self.assertTrue(all(issue.channel for issue in caught.exception.issues))

    def test_duplicate_instrument(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            manifest = self._case(directory)
            value = _load_manifest(manifest)
            value["channels"][1]["instrument"] = value["channels"][0]["instrument"]
            _write_manifest(manifest, value)
            with self.assertRaises(ValidationFailure) as caught:
                load_project(manifest)
            issues = [
                issue
                for issue in caught.exception.issues
                if issue.code == "DUPLICATE_INSTRUMENT"
            ]
            self.assertEqual({issue.channel for issue in issues}, {"board_drum", "bangzi"})

    def test_nonascending_reference_times(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            manifest = self._case(directory)
            value = _load_manifest(manifest)
            value["channels"][0]["reference_times"] = [0.5, 0.2, 0.8]
            _write_manifest(manifest, value)
            with self.assertRaises(ValidationFailure) as caught:
                load_project(manifest)
            issues = [
                issue
                for issue in caught.exception.issues
                if issue.code == "REFERENCE_TIMES_NOT_ASCENDING"
            ]
            self.assertEqual(issues[0].channel, "board_drum")

    def test_cli_aborts_and_writes_structured_report(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            manifest = self._case(directory)
            (Path(directory) / "tanggu.wav").unlink()
            output = Path(directory) / "output"
            process = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "drum_bus",
                    "render",
                    "--manifest",
                    str(manifest),
                    "--output",
                    str(output),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(process.returncode, 2)
            self.assertIn("[FILE_MISSING]", process.stderr)
            self.assertIn("channel=tanggu", process.stderr)
            report = json.loads((output / "validation_report.json").read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "aborted_validation")
            self.assertIn("FILE_MISSING", {item["code"] for item in report["warnings"]})
            self.assertFalse((output / "bus.wav").exists())


if __name__ == "__main__":
    unittest.main()

