from __future__ import annotations

import tempfile
import unittest

from tests.helpers import sample_path
from vp_pipeline.checkpoint import CheckpointStore
from vp_pipeline.errors import E_PARA, PipelineError
from vp_pipeline.loader import load_scene
from vp_pipeline.session import CompositorSession


class ConcurrencyTests(unittest.TestCase):
    def _run_sequence(self, directory: str, parallel: bool) -> tuple:
        model = load_scene(sample_path(3))
        session = CompositorSession(
            model,
            CheckpointStore(directory),
            max_workers=4,
            max_frame_buffers_per_thread=1,
        )
        first = session.run(parallel=parallel, max_workers=4)
        session.apply_edit("prop_lamp", "opacity", 0.37)
        session.apply_edit("character_glow", "visible", False)
        second = session.run(parallel=parallel, max_workers=4)
        hashes = tuple(
            (index, frame.content_hash)
            for index, frame in sorted(session.current_frames().items())
        )
        return (
            first.commit_order,
            second.commit_order,
            second.reused_windows,
            hashes,
            session.compare_with_full().ok,
        )

    def test_parallel_and_serial_edit_sequences_are_identical(self) -> None:
        with tempfile.TemporaryDirectory() as serial_directory, tempfile.TemporaryDirectory() as parallel_directory:
            serial = self._run_sequence(serial_directory, False)
            parallel = self._run_sequence(parallel_directory, True)
        self.assertEqual(serial, parallel)

    def test_single_window_failure_is_isolated_and_retryable(self) -> None:
        model = load_scene(sample_path(2))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory), max_workers=3)

            def fail_frame_four(frame_index: int) -> None:
                if frame_index == 4:
                    raise RuntimeError("injected frame failure")

            result = session.run(parallel=True, frame_hook=fail_frame_four)
            self.assertEqual(result.failed_windows, (1,))
            self.assertEqual(result.committed_count, 5)
            self.assertEqual(session.windows[1].state.value, "failed")
            self.assertTrue(
                all(
                    session.windows[index].state.value == "committed"
                    for index in (0, 2, 3, 4, 5)
                )
            )
            retry = session.run(parallel=True)
            self.assertEqual(retry.commit_order, (1,))
            self.assertEqual(retry.failed_windows, ())
            self.assertTrue(session.compare_with_full().ok)

    def test_resource_bounds_return_e_para(self) -> None:
        model = load_scene(sample_path(1))
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(PipelineError) as caught:
                CompositorSession(
                    model,
                    CheckpointStore(directory),
                    max_workers=99,
                )
            self.assertEqual(caught.exception.code, E_PARA)
            self.assertEqual(caught.exception.field_path, "max_workers")
            session = CompositorSession(model, CheckpointStore(directory))
            with self.assertRaises(PipelineError) as caught:
                session.run(parallel=True, max_workers=0)
            self.assertEqual(caught.exception.code, E_PARA)
            self.assertEqual(caught.exception.field_path, "max_workers")


if __name__ == "__main__":
    unittest.main()
