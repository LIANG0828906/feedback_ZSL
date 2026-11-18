from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from bamboo_render.engine import run_render
from bamboo_render.sample_scene import make_sample_scene
from bamboo_render.scene import load_scene_data


def snapshot_scene() -> dict:
    return make_sample_scene(
        image_width=128,
        image_height=96,
        chunk_width=64,
        chunk_height=48,
    )


class SnapshotResumeTests(unittest.TestCase):
    def test_resume_matches_one_shot_render_byte_for_byte(self) -> None:
        loaded = load_scene_data(snapshot_scene())
        with tempfile.TemporaryDirectory() as full_dir, tempfile.TemporaryDirectory() as resume_dir:
            run_render(loaded, full_dir)
            interrupted = run_render(loaded, resume_dir, interrupt_after=1)
            self.assertEqual(interrupted.status, "interrupted")
            self.assertEqual(interrupted.completed_blocks, 1)
            recovery = json.loads(
                (Path(resume_dir) / "recovery_report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(recovery["status"], "interrupted")
            resumed = run_render(loaded, resume_dir)
            self.assertEqual(resumed.status, "complete")
            self.assertEqual(resumed.reused_blocks, 1)
            self.assertEqual(resumed.recomputed_blocks, 3)
            full_path = Path(full_dir)
            resume_path = Path(resume_dir)
            for filename in (
                "rgba.npy",
                "ownership.npy",
                "layer_premultiplied.npy",
                "snapshot.json",
                "run_report.json",
                "projection_report.json",
                "projection_points.json",
            ):
                self.assertEqual(
                    (full_path / filename).read_bytes(),
                    (resume_path / filename).read_bytes(),
                    filename,
                )
            np.testing.assert_array_equal(
                np.load(full_path / "rgba.npy"),
                np.load(resume_path / "rgba.npy"),
            )
            recovery = json.loads(
                (resume_path / "recovery_report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(recovery["status"], "resumed")
            self.assertFalse(recovery["snapshot_discarded"])
            report_text = (resume_path / "recovery_report.json").read_text(encoding="utf-8")
            self.assertNotIn("timestamp", report_text)
            self.assertNotIn("/home/", report_text)

    def test_stale_block_offsets_discard_entire_snapshot(self) -> None:
        loaded = load_scene_data(snapshot_scene())
        with tempfile.TemporaryDirectory() as full_dir, tempfile.TemporaryDirectory() as resume_dir:
            run_render(loaded, full_dir)
            run_render(loaded, resume_dir, interrupt_after=2)
            snapshot_file = Path(resume_dir) / "snapshot.json"
            snapshot = json.loads(snapshot_file.read_text(encoding="utf-8"))
            snapshot["completed_blocks"][0]["x"] += 1
            snapshot_file.write_text(
                json.dumps(snapshot, sort_keys=True, separators=(",", ":")),
                encoding="utf-8",
            )
            resumed = run_render(loaded, resume_dir)
            self.assertEqual(resumed.reused_blocks, 0)
            recovery = json.loads(
                (Path(resume_dir) / "recovery_report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(recovery["discard_reason"], "SNAPSHOT_BLOCK_OFFSET_MISMATCH")
            self.assertEqual(
                (Path(full_dir) / "rgba.npy").read_bytes(),
                (Path(resume_dir) / "rgba.npy").read_bytes(),
            )

    def test_scene_digest_mismatch_discards_snapshot(self) -> None:
        original = load_scene_data(snapshot_scene())
        changed_raw = snapshot_scene()
        changed_raw["seed"] += 1
        changed = load_scene_data(changed_raw)
        with tempfile.TemporaryDirectory() as output_dir:
            run_render(original, output_dir, interrupt_after=1)
            resumed = run_render(changed, output_dir)
            self.assertEqual(resumed.reused_blocks, 0)
            recovery = json.loads(
                (Path(output_dir) / "recovery_report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(recovery["discard_reason"], "SNAPSHOT_SCENE_MISMATCH")
            self.assertEqual(recovery["status"], "recomputed")

    def test_corrupt_snapshot_payload_is_discarded(self) -> None:
        loaded = load_scene_data(snapshot_scene())
        with tempfile.TemporaryDirectory() as full_dir, tempfile.TemporaryDirectory() as resume_dir:
            run_render(loaded, full_dir)
            run_render(loaded, resume_dir, interrupt_after=1)
            snapshot_file = Path(resume_dir) / "snapshot.json"
            snapshot = json.loads(snapshot_file.read_text(encoding="utf-8"))
            snapshot["completed_blocks"][0]["rgba_data"] = (
                snapshot["completed_blocks"][0]["rgba_data"][:-8] + "AAAAAAA="
            )
            snapshot_file.write_text(
                json.dumps(snapshot, sort_keys=True, separators=(",", ":")),
                encoding="utf-8",
            )
            resumed = run_render(loaded, resume_dir)
            self.assertEqual(resumed.reused_blocks, 0)
            recovery = json.loads(
                (Path(resume_dir) / "recovery_report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(recovery["discard_reason"], "SNAPSHOT_CORRUPT")
            self.assertEqual(
                (Path(full_dir) / "rgba.npy").read_bytes(),
                (Path(resume_dir) / "rgba.npy").read_bytes(),
            )


if __name__ == "__main__":
    unittest.main()
