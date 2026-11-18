from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import numpy as np

from celadon_museum.errors import ResumeMismatchError
from celadon_museum.meshgen import make_scene
from celadon_museum.pipeline import render_scene
from celadon_museum.scene import validate_scene


def resume_scene():
    return validate_scene(
        make_scene(
            width=128,
            height=96,
            tile_size=32,
            texture_resolution=64,
            radial_segments=20,
            rings=5,
        )
    )


class ResumeTests(unittest.TestCase):
    def test_interrupted_then_resumed_matches_uninterrupted(self):
        scene = resume_scene()
        with tempfile.TemporaryDirectory() as full_dir, tempfile.TemporaryDirectory() as resumed_dir:
            reference = render_scene(scene, full_dir, install_signal_handlers=False)
            interrupted = render_scene(
                scene,
                resumed_dir,
                stop_after_tiles=4,
                install_signal_handlers=False,
            )
            self.assertEqual(interrupted.exit_code, 0)
            self.assertEqual(interrupted.status, "interrupted")
            resume_report = json.loads(
                (Path(resumed_dir) / "resume_report.json").read_text()
            )
            self.assertEqual(len(resume_report["completed_tile_ids"]), 4)
            self.assertEqual(resume_report["next_tile_id"], 4)
            resumed = render_scene(scene, resumed_dir, install_signal_handlers=False)
            self.assertEqual(resumed.status, "complete")
            self.assertTrue(resumed.report["verification"]["performed"])
            self.assertTrue(resumed.report["verification"]["match"])
            self.assertEqual(resumed.report["recovery"]["resume_count"], 1)
            self.assertEqual(
                (Path(full_dir) / "rgba.npy").read_bytes(),
                (Path(resumed_dir) / "rgba.npy").read_bytes(),
            )
            self.assertEqual(
                (Path(full_dir) / "optical_depth.npy").read_bytes(),
                (Path(resumed_dir) / "optical_depth.npy").read_bytes(),
            )
            self.assertEqual(
                reference.report["image"]["digest"],
                resumed.report["image"]["digest"],
            )

    def test_corrupt_snapshot_is_discarded_entirely(self):
        scene = resume_scene()
        with tempfile.TemporaryDirectory() as reference_dir, tempfile.TemporaryDirectory() as output:
            render_scene(scene, reference_dir, install_signal_handlers=False)
            render_scene(
                scene,
                output,
                stop_after_tiles=3,
                install_signal_handlers=False,
            )
            (Path(output) / ".state" / "snapshot.json").write_text("{broken", encoding="utf-8")
            result = render_scene(scene, output, install_signal_handlers=False)
            self.assertEqual(result.status, "complete")
            self.assertEqual(result.report["recovery"]["discarded_snapshots"], 1)
            self.assertEqual(result.report["recovery"]["resume_count"], 0)
            self.assertFalse(result.report["verification"]["performed"])
            self.assertEqual(
                (Path(reference_dir) / "rgba.npy").read_bytes(),
                (Path(output) / "rgba.npy").read_bytes(),
            )

    def test_scene_digest_mismatch_forces_fresh_render(self):
        scene = resume_scene()
        changed_document = json.loads(json.dumps(scene.document))
        changed_document["image"]["background"] = [0.02, 0.03, 0.04]
        changed_scene = validate_scene(changed_document)
        with tempfile.TemporaryDirectory() as output:
            render_scene(
                scene,
                output,
                stop_after_tiles=3,
                install_signal_handlers=False,
            )
            result = render_scene(changed_scene, output, install_signal_handlers=False)
            self.assertEqual(result.report["recovery"]["discarded_snapshots"], 1)
            self.assertEqual(result.report["recovery"]["resume_count"], 0)
            self.assertEqual(result.report["tiles"]["completed_count"], result.report["tiles"]["total"])

    def test_resume_mismatch_emits_pixel_and_channel_difference(self):
        scene = resume_scene()
        with tempfile.TemporaryDirectory() as output:
            render_scene(scene, output, install_signal_handlers=False)

            from celadon_museum.pipeline import render_tile as original_render_tile

            def drifted_render_tile(context, tile_id, row, col):
                rgba, depth, projection, stats = original_render_tile(
                    context, tile_id, row, col
                )
                rgba = rgba.copy()
                rgba[0, 0, 0] = int(rgba[0, 0, 0]) ^ 1
                return rgba, depth, projection, stats

            with mock.patch(
                "celadon_museum.pipeline.render_tile",
                side_effect=drifted_render_tile,
            ):
                with self.assertRaises(ResumeMismatchError):
                    render_scene(scene, output, install_signal_handlers=False)
            diff = json.loads((Path(output) / "resume_diff.json").read_text())
            error = diff["error"]
            self.assertEqual(error["code"], "E_RESUME_MISMATCH")
            self.assertGreater(error["difference_count"], 0)
            first = error["differences"][0]
            self.assertIn("tile_id", first)
            self.assertEqual(first["pixel"], [0, 0])
            self.assertEqual(first["channel"], "r")


if __name__ == "__main__":
    unittest.main()
