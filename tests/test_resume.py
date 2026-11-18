from __future__ import annotations

import hashlib
import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np
from PIL import Image

from fogvol.pipeline import run_render
from fogvol.scene import parse_scene
from fogvol.snapshot import SNAPSHOT_NAME, load_snapshot, write_snapshot

from .common import ROOT, compact_scene


OUTPUT_FILES = (
    "rgba.png",
    "rgba.npy",
    "optical_depth.npy",
    "optical_depth.png",
    "opacity.npy",
    "opacity.png",
    "radiance.npy",
)


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class ResumeTests(unittest.TestCase):
    def test_snapshot_only_resume_is_byte_exact(self) -> None:
        scene = parse_scene(compact_scene())
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            full = root / "full"
            interrupted = root / "interrupted"
            hardkill = root / "hardkill"

            full_result = run_render(scene, full)
            self.assertEqual(full_result.status, "complete")

            interrupted_result = run_render(
                scene, interrupted, interrupt_after_chunks=3
            )
            self.assertEqual(interrupted_result.status, "interrupted")
            self.assertTrue((interrupted / SNAPSHOT_NAME).exists())
            self.assertFalse((interrupted / "rgba.png").exists())
            self.assertTrue((interrupted / "recovery_report.json").exists())
            with np.load(
                interrupted / SNAPSHOT_NAME, allow_pickle=False
            ) as archive:
                manifest = json.loads(
                    archive["manifest"].tobytes().decode("utf-8")
                )
            self.assertEqual(manifest["completed_chunk_count"], 3)
            self.assertEqual(
                manifest["completed_chunk_ids"],
                ["r000_c000", "r000_c001", "r000_c002"],
            )
            self.assertEqual(
                set(manifest["chunk_color_summaries"]),
                set(manifest["completed_chunk_ids"]),
            )

            hardkill.mkdir()
            shutil.copyfile(
                interrupted / SNAPSHOT_NAME,
                hardkill / SNAPSHOT_NAME,
            )
            resumed_result = run_render(scene, hardkill)
            self.assertEqual(resumed_result.status, "complete")
            report = json.loads(
                (hardkill / "report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(report["resume"]["recovery_count"], 1)
            self.assertEqual(
                report["resume"]["verification"]["reused_chunks"], 3
            )
            self.assertTrue(report["resume"]["verification"]["exact"])

            for name in OUTPUT_FILES:
                self.assertEqual(
                    sha(full / name),
                    sha(hardkill / name),
                    msg=name,
                )
            self.assertFalse((hardkill / SNAPSHOT_NAME).exists())

    def test_report_is_stable_and_has_no_timestamp_or_absolute_path(self) -> None:
        scene = parse_scene(compact_scene())
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            out = root / "out"
            result = run_render(scene, out)
            self.assertEqual(result.status, "complete")
            text = (out / "report.json").read_text(encoding="utf-8")
            report = json.loads(text)
            self.assertEqual(
                list(report.keys()), sorted(report.keys())
            )
            self.assertNotIn("timestamp", text.lower())
            self.assertNotIn(str(ROOT), text)
            self.assertNotIn(str(root), text)
            self.assertIn("lighting", report)
            self.assertIn("clipped_pixels", report["tonemapping"])
            self.assertIn("recovery_count", report["resume"])
            rgba = np.load(out / "rgba.npy", allow_pickle=False)
            depth = np.load(
                out / "optical_depth.npy", allow_pickle=False
            )
            opacity = np.load(out / "opacity.npy", allow_pickle=False)
            self.assertEqual(rgba.shape, (60, 80, 4))
            self.assertEqual(depth.shape, (60, 80))
            self.assertEqual(opacity.shape, (60, 80))
            self.assertGreaterEqual(float(rgba.min()), 0.0)
            self.assertLessEqual(float(rgba.max()), 1.0)
            with Image.open(out / "rgba.png") as image:
                self.assertEqual(image.info["step"], "1")
                self.assertEqual(image.info["substeps"], "2")
                self.assertEqual(image.info["integration_step"], "0.5")
                self.assertIn(
                    "ray_box_entry", image.info["ray_start_rule"]
                )

    def test_scene_mismatch_discards_snapshot(self) -> None:
        raw = compact_scene()
        scene = parse_scene(raw)
        with tempfile.TemporaryDirectory() as temporary:
            out = Path(temporary) / "out"
            interrupted = run_render(
                scene, out, interrupt_after_chunks=2
            )
            self.assertEqual(interrupted.status, "interrupted")
            changed_raw = compact_scene()
            changed_raw["camera"]["target"][1] += 0.25
            changed = parse_scene(changed_raw)
            result = run_render(
                changed, out, interrupt_after_chunks=1
            )
            self.assertEqual(result.status, "interrupted")
            recovery = json.loads(
                (out / "recovery_report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(recovery["completed_chunk_count"], 1)
            loaded = load_snapshot(out / SNAPSHOT_NAME, changed)
            self.assertEqual(loaded.status, "loaded")

    def test_corrupt_snapshot_is_discarded(self) -> None:
        scene = parse_scene(compact_scene())
        with tempfile.TemporaryDirectory() as temporary:
            out = Path(temporary) / "out"
            out.mkdir()
            (out / SNAPSHOT_NAME).write_bytes(b"not a zip archive")
            result = run_render(scene, out)
            self.assertEqual(result.status, "complete")
            report = json.loads(
                (out / "report.json").read_text(encoding="utf-8")
            )
            self.assertTrue(report["resume"]["snapshot_discarded"])
            self.assertEqual(report["resume"]["reused_chunks"], 0)

    def test_resume_mismatch_writes_structured_difference(self) -> None:
        scene = parse_scene(compact_scene())
        with tempfile.TemporaryDirectory() as temporary:
            out = Path(temporary) / "out"
            run_render(scene, out, interrupt_after_chunks=3)
            snapshot_path = out / SNAPSHOT_NAME
            loaded = load_snapshot(snapshot_path, scene)
            self.assertIsNotNone(loaded.state)
            loaded.state.radiance[0, 0, 0] += 1.0
            write_snapshot(snapshot_path, loaded.state, scene)

            result = run_render(scene, out)
            self.assertEqual(result.status, "resume_mismatch")
            difference = json.loads(
                (out / "resume_diff.json").read_text(encoding="utf-8")
            )
            self.assertEqual(
                difference["error"]["code"], "RESUME_PIXEL_MISMATCH"
            )
            self.assertEqual(difference["error"]["pixel"], [0, 0])
            self.assertEqual(difference["error"]["chunk_id"], "r000_c000")
            self.assertIn("R", difference["error"]["channels"])
            self.assertFalse((out / "rgba.png").exists())


if __name__ == "__main__":
    unittest.main()

