from __future__ import annotations

import dataclasses
import tempfile
import unittest

from tests.helpers import sample_path
from vp_pipeline.checkpoint import CheckpointStore
from vp_pipeline.diff import assert_frames_equal, first_difference
from vp_pipeline.errors import E_DIFF, PipelineError
from vp_pipeline.loader import load_scene
from vp_pipeline.renderer import render_all_frames
from vp_pipeline.session import CompositorSession


class IncrementalTests(unittest.TestCase):
    def test_incremental_invalidation_and_pixel_equality(self) -> None:
        model = load_scene(sample_path(3))
        with tempfile.TemporaryDirectory() as directory:
            first = CompositorSession(model, CheckpointStore(directory))
            initial = first.run(parallel=True)
            self.assertEqual(initial.commit_order, (0, 1, 2, 3, 4, 5))
            restarted = CompositorSession(model, CheckpointStore(directory))
            self.assertEqual(len(restarted.recovery.restored), 6)
            edited = restarted.apply_edit("prop_crate", "opacity", 0.41)
            composite_windows = [
                item.window_index
                for item in edited.invalidations
                if item.stage == "composite"
            ]
            self.assertEqual(composite_windows, [0, 1, 2])
            self.assertTrue(
                all(
                    item.layer_ids == ("prop_crate",)
                    for item in edited.invalidations
                    if item.stage == "composite"
                )
            )
            result = restarted.run(parallel=True)
            self.assertEqual(result.commit_order, (0, 1, 2))
            self.assertEqual(result.reused_windows, (3, 4, 5))
            full = render_all_frames(restarted.model)
            assert_frames_equal(result.frames, full, restarted.model)
            self.assertTrue(restarted.compare_with_full().ok)

    def test_visibility_edit_only_invalidates_overlapping_windows(self) -> None:
        model = load_scene(sample_path(2))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory))
            session.run(parallel=False)
            edit = session.apply_edit("prop_lamp", "visible", False)
            windows = [
                item.window_index
                for item in edit.invalidations
                if item.stage == "composite"
            ]
            self.assertEqual(windows, [1, 2, 3, 4])
            result = session.run(parallel=False)
            self.assertEqual(result.commit_order, (1, 2, 3, 4))
            self.assertEqual(result.reused_windows, (0, 5))
            assert_frames_equal(
                result.frames, render_all_frames(session.model), session.model
            )

    def test_timebase_edit_invalidates_all_windows(self) -> None:
        model = load_scene(sample_path(1))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory))
            session.run(parallel=False)
            edit = session.set_frame_rate(25, 1)
            windows = [
                item.window_index
                for item in edit.invalidations
                if item.stage == "composite"
            ]
            self.assertEqual(windows, [0, 1, 2, 3, 4, 5])
            self.assertTrue(
                all(
                    len(item.layer_ids) == 5
                    for item in edit.invalidations
                    if item.stage == "composite"
                )
            )
            result = session.run(parallel=False)
            self.assertEqual(result.reused_windows, ())
            self.assertEqual(
                (session.model.timeline.fps_num, session.model.timeline.fps_den),
                (25, 1),
            )

    def test_diff_locates_first_pixel_window_and_layer(self) -> None:
        model = load_scene(sample_path(3))
        frames = render_all_frames(model)
        broken = dict(frames)
        target = frames[4]
        pixels = target.pixels.copy()
        pixels[16, 6, 2] = (int(pixels[16, 6, 2]) + 23) % 256
        broken[4] = dataclasses.replace(target, pixels=pixels)
        report = first_difference(broken, frames, model)
        self.assertIsNotNone(report)
        assert report is not None
        self.assertEqual(report.frame_index, 4)
        self.assertEqual(report.window_index, 1)
        self.assertEqual((report.x, report.y), (6, 16))
        with self.assertRaises(PipelineError) as caught:
            assert_frames_equal(broken, frames, model)
        self.assertEqual(caught.exception.code, E_DIFF)
        self.assertEqual(caught.exception.details["window_index"], 1)
        self.assertEqual(caught.exception.details["x"], 6)
        self.assertEqual(caught.exception.details["y"], 16)


if __name__ == "__main__":
    unittest.main()
