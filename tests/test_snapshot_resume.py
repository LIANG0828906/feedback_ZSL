import json
import os
import shutil
import tempfile
import unittest

import numpy as np

from brocade_embroidery.render import render_frame
from brocade_embroidery.scene_gen import generate_scene


class SnapshotResumeTests(unittest.TestCase):
    def setUp(self):
        self.scene = generate_scene(small=True)
        self.temp = tempfile.mkdtemp(prefix="brocade-test-")

    def tearDown(self):
        shutil.rmtree(self.temp, ignore_errors=True)

    def _paths(self, frame_dir):
        return {
            "png": os.path.join(frame_dir, "render.rgba.png"),
            "owner": os.path.join(frame_dir, "ownership.u16"),
            "report": os.path.join(frame_dir, "report.json"),
        }

    def test_two_full_runs_are_byte_identical(self):
        first_dir = os.path.join(self.temp, "first")
        second_dir = os.path.join(self.temp, "second")
        first = render_frame(self.scene, first_dir, 0, 0.0,
                             allow_resume=False)
        second = render_frame(self.scene, second_dir, 0, 0.0,
                              allow_resume=False)
        np.testing.assert_array_equal(first.rgba, second.rgba)
        np.testing.assert_array_equal(first.ownership, second.ownership)
        first_paths = self._paths(os.path.join(first_dir, "frame_00"))
        second_paths = self._paths(os.path.join(second_dir, "frame_00"))
        for key in ("png", "owner"):
            with open(first_paths[key], "rb") as a, open(second_paths[key], "rb") as b:
                self.assertEqual(a.read(), b.read())

    def test_sigint_style_partial_then_resume_matches_full_run(self):
        reference_dir = os.path.join(self.temp, "reference")
        partial_dir = os.path.join(self.temp, "resume")
        reference = render_frame(self.scene, reference_dir, 0, 0.0,
                                 allow_resume=False)
        partial = render_frame(self.scene, partial_dir, 0, 0.0,
                               allow_resume=False, max_tiles=2)
        self.assertTrue(partial.report["incomplete"])
        self.assertFalse(os.path.exists(
            os.path.join(partial_dir, "frame_00", "render.rgba.png")))
        resumed = render_frame(self.scene, partial_dir, 0, 0.0,
                               allow_resume=True)
        np.testing.assert_array_equal(reference.rgba, resumed.rgba)
        np.testing.assert_array_equal(reference.ownership, resumed.ownership)
        reference_paths = self._paths(os.path.join(reference_dir, "frame_00"))
        resumed_paths = self._paths(os.path.join(partial_dir, "frame_00"))
        for key in ("png", "owner"):
            with open(reference_paths[key], "rb") as a, open(resumed_paths[key], "rb") as b:
                self.assertEqual(a.read(), b.read())
        report = resumed.report
        self.assertEqual(len(report["blocks"]),
                         len(reference.report["blocks"]))

    def test_corrupt_snapshot_is_discarded_and_rerun_from_zero(self):
        work_dir = os.path.join(self.temp, "corrupt")
        render_frame(self.scene, work_dir, 0, 0.0,
                     allow_resume=False, max_tiles=2)
        snapshot_path = os.path.join(
            work_dir, "frame_00", "snapshot", "snapshot.json")
        with open(snapshot_path, "rb") as handle:
            payload = json.loads(handle.read().decode("utf-8"))
        payload["scene_digest"] = "deadbeef"
        with open(snapshot_path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)
        resumed = render_frame(self.scene, work_dir, 0, 0.0,
                               allow_resume=True)
        self.assertTrue(resumed.report["recovery"]["snapshot_restarted"])
        self.assertEqual(
            resumed.report["recovery"]["completed_tiles"],
            len(resumed.report["blocks"]))

    def test_seed_change_discards_old_snapshot(self):
        work_dir = os.path.join(self.temp, "seed-change")
        render_frame(self.scene, work_dir, 0, 0.0,
                     allow_resume=False, max_tiles=1)
        changed = generate_scene(small=True, seed=20260102)
        resumed = render_frame(changed, work_dir, 0, 0.0,
                               allow_resume=True)
        self.assertTrue(resumed.report["recovery"]["snapshot_restarted"])
        self.assertFalse(resumed.report.get("incomplete", False))


if __name__ == "__main__":
    unittest.main()
