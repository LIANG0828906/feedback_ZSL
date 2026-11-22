from __future__ import annotations

import tempfile
import unittest

from tests.helpers import sample_path
from vp_pipeline.checkpoint import CheckpointStore
from vp_pipeline.errors import E_STATE, PipelineError
from vp_pipeline.loader import load_scene
from vp_pipeline.session import CompositorSession
from vp_pipeline.state import WindowSlot, WindowState, transition


class StateMachineTests(unittest.TestCase):
    def test_illegal_transition_is_rejected_without_mutation(self) -> None:
        slot = WindowSlot(index=0, start=0, end_exclusive=3)
        with self.assertRaises(PipelineError) as caught:
            transition(slot, WindowState.COMMITTED)
        self.assertEqual(caught.exception.code, E_STATE)
        self.assertEqual(slot.state, WindowState.PENDING)
        transition(slot, WindowState.COMPUTING)
        transition(slot, WindowState.COMMITTED)
        with self.assertRaises(PipelineError) as caught:
            transition(slot, WindowState.COMPUTING)
        self.assertEqual(caught.exception.code, E_STATE)
        self.assertEqual(slot.state, WindowState.COMMITTED)

    def test_committed_snapshot_and_frames_are_not_silently_rewritten(self) -> None:
        model = load_scene(sample_path(3))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory))
            session.run(parallel=True)
            before = session.windows[1]
            old_snapshot = before.snapshot
            old_hashes = tuple(frame.content_hash for frame in before.frames)
            edit = session.apply_edit("prop_crate", "opacity", 0.2)
            self.assertIn(1, [item.window_index for item in edit.invalidations])
            after = session.windows[1]
            self.assertEqual(after.state, WindowState.PENDING)
            self.assertEqual(after.snapshot, old_snapshot)
            self.assertEqual(
                tuple(frame.content_hash for frame in after.frames), old_hashes
            )
            self.assertEqual(session.progress, (3, 6))

    def test_progress_counts_committed_windows(self) -> None:
        model = load_scene(sample_path(1))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory))
            session.run(parallel=False)
            self.assertEqual(session.progress, (6, 6))
            session.apply_edit("character", "visible", False)
            self.assertLess(session.progress[0], 6)


if __name__ == "__main__":
    unittest.main()
