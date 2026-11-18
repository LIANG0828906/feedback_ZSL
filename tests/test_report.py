from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from shadowbox.pipeline import render_to_directory
from shadowbox.scene import load_scene

from tests.helpers import small_scene, write_scene_dict


class ReportTests(unittest.TestCase):
    def test_block_summaries_are_recomputable_from_final_pixels(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            scene_path = directory / "scene.json"
            write_scene_dict(scene_path, small_scene())
            scene = load_scene(scene_path)
            output = directory / "output"
            result = render_to_directory(scene, output, fresh=True)
            self.assertEqual(result.exit_code, 0)
            report = json.loads(
                (output / "report.json").read_text(encoding="utf-8")
            )
            raw = (output / "rgba.raw").read_bytes()
            rgba = np.frombuffer(raw, dtype=np.uint8).reshape(
                scene.camera.height, scene.camera.width, 4
            )
            self.assertEqual(
                report["summary"]["image_rgba_sha256"],
                hashlib.sha256(raw).hexdigest(),
            )
            self.assertEqual(len(report["layers"]), 5)
            self.assertEqual(len(report["blocks"]), 9)
            for block in report["blocks"]:
                x = block["x"]
                y = block["y"]
                width = block["width"]
                height = block["height"]
                pixels = rgba[y : y + height, x : x + width]
                self.assertEqual(
                    block["rgba_sha256"],
                    hashlib.sha256(pixels.tobytes(order="C")).hexdigest(),
                )
                expected_mean = np.mean(
                    pixels.astype(np.float64) / 255.0, axis=(0, 1)
                )
                np.testing.assert_allclose(
                    block["mean_rgba"],
                    expected_mean,
                    atol=5e-7,
                )
            serialized = json.dumps(report, ensure_ascii=False)
            self.assertNotIn(str(directory), serialized)
            self.assertNotIn("timestamp", serialized)
            self.assertNotIn("duration", serialized)
            gaps = [probe["air_gap_mm"] for probe in report["penumbra_probes"]]
            self.assertEqual(
                gaps,
                sorted(probe["air_gap_mm"] for probe in report["penumbra_probes"]),
            )
            widths = [
                probe["half_penumbra_width_mm"]
                for probe in report["penumbra_probes"]
            ]
            self.assertTrue(
                all(left < right for left, right in zip(widths, widths[1:])),
                widths,
            )

    def test_no_absolute_paths_in_resume_report(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            scene_path = directory / "scene.json"
            write_scene_dict(scene_path, small_scene())
            scene = load_scene(scene_path)
            output = directory / "output"
            result = render_to_directory(
                scene,
                output,
                fresh=True,
                interrupt_after_blocks=1,
            )
            self.assertEqual(result.exit_code, 0)
            payload = (output / "resume_report.json").read_text(encoding="utf-8")
            self.assertNotIn(str(directory), payload)
            parsed = json.loads(payload)
            self.assertEqual(parsed["checkpoint_state"], "partial")
            self.assertEqual(len(parsed["completed_blocks"]), 1)


if __name__ == "__main__":
    unittest.main()
