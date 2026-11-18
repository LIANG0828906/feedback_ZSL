from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

from bamboo_render.engine import run_render
from bamboo_render.sample_scene import make_sample_scene
from bamboo_render.scene import load_scene_data


ROOT = Path(__file__).resolve().parents[1]


class CompareAndCliTests(unittest.TestCase):
    def _render(self, directory: str) -> None:
        raw = make_sample_scene(
            image_width=128,
            image_height=96,
            chunk_width=64,
            chunk_height=48,
        )
        run_render(load_scene_data(raw), directory)

    def test_compare_reports_pixel_chunk_and_channels(self) -> None:
        with tempfile.TemporaryDirectory() as reference_dir, tempfile.TemporaryDirectory() as candidate_dir:
            self._render(reference_dir)
            self._render(candidate_dir)
            candidate_file = Path(candidate_dir) / "rgba.npy"
            rgba = np.load(candidate_file)
            rgba[5, 70, 1] = int(rgba[5, 70, 1]) ^ 0xFF
            np.save(candidate_file, rgba, allow_pickle=False)
            report_file = Path(candidate_dir) / "compare-report.json"
            result = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "bamboo_render",
                    "compare",
                    "--reference",
                    reference_dir,
                    "--candidate",
                    candidate_dir,
                    "--report",
                    str(report_file),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
                env={**os.environ, "PYTHONPATH": str(ROOT)},
            )
            self.assertEqual(result.returncode, 1)
            report = json.loads(report_file.read_text(encoding="utf-8"))
            self.assertFalse(report["ok"])
            pixel_differences = [
                item for item in report["differences"] if item["kind"] == "pixel_mismatch"
            ]
            self.assertTrue(pixel_differences)
            first = pixel_differences[0]
            self.assertEqual(first["x"], 70)
            self.assertEqual(first["y"], 5)
            self.assertIn("G", first["channels"])
            self.assertEqual(first["chunk_id"], 1)

    def test_cli_validation_error_is_structured_and_nonzero(self) -> None:
        raw = make_sample_scene(
            image_width=32,
            image_height=24,
            chunk_width=16,
            chunk_height=12,
        )
        raw["layers"][1]["mesh"]["triangles"][0][0] = 99999
        with tempfile.TemporaryDirectory() as directory:
            scene_file = Path(directory) / "bad.json"
            scene_file.write_text(json.dumps(raw), encoding="utf-8")
            result = subprocess.run(
                [sys.executable, "-m", "bamboo_render", "validate", str(scene_file)],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
                env={**os.environ, "PYTHONPATH": str(ROOT)},
            )
            self.assertEqual(result.returncode, 2)
            payload = json.loads(result.stderr)
            self.assertFalse(payload["ok"])
            self.assertEqual(payload["error"]["code"], "MESH_INDEX_OUT_OF_RANGE")
            self.assertEqual(payload["error"]["layer"], 2)

    def test_cli_interrupt_exits_zero_and_resume_exits_zero(self) -> None:
        raw = make_sample_scene(
            image_width=128,
            image_height=96,
            chunk_width=64,
            chunk_height=48,
        )
        with tempfile.TemporaryDirectory() as directory:
            scene_file = Path(directory) / "scene.json"
            scene_file.write_text(json.dumps(raw), encoding="utf-8")
            output_dir = Path(directory) / "out"
            first = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "bamboo_render",
                    "render",
                    str(scene_file),
                    "--out",
                    str(output_dir),
                    "--interrupt-after",
                    "1",
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
                env={**os.environ, "PYTHONPATH": str(ROOT)},
            )
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertEqual(json.loads(first.stdout)["status"], "interrupted")
            second = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "bamboo_render",
                    "render",
                    str(scene_file),
                    "--out",
                    str(output_dir),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
                env={**os.environ, "PYTHONPATH": str(ROOT)},
            )
            self.assertEqual(second.returncode, 0, second.stderr)
            payload = json.loads(second.stdout)
            self.assertEqual(payload["status"], "complete")
            self.assertGreater(payload["blocks_reused"], 0)

    def test_verify_resume_command_returns_zero_on_byte_equality(self) -> None:
        raw = make_sample_scene(
            image_width=128,
            image_height=96,
            chunk_width=64,
            chunk_height=48,
        )
        with tempfile.TemporaryDirectory() as directory:
            scene_file = Path(directory) / "scene.json"
            scene_file.write_text(json.dumps(raw), encoding="utf-8")
            result = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "bamboo_render",
                    "verify-resume",
                    str(scene_file),
                    "--out",
                    str(Path(directory) / "verification"),
                    "--interrupt-after",
                    "1",
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
                env={**os.environ, "PYTHONPATH": str(ROOT)},
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            payload = json.loads(result.stdout)
            self.assertTrue(payload["ok"])
            self.assertEqual(payload["difference_count"], 0)
            self.assertGreater(payload["blocks_reused"], 0)


if __name__ == "__main__":
    unittest.main()
