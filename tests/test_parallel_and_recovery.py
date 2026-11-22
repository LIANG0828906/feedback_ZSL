from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import numpy as np

from npr_workbench.engine import NprEngine, RenderControl
import npr_workbench.engine as engine_module
from npr_workbench.tiles import FaultInjector

from tests.helpers import fast_params, sample_scene


def _engine(scene: object, params: object, directory: str, session: str) -> NprEngine:
    return NprEngine(
        scene,  # type: ignore[arg-type]
        params,  # type: ignore[arg-type]
        scene.camera_poses[0],  # type: ignore[attr-defined]
        persist_dir=Path(directory) / "persist",
        diagnostics_dir=Path(directory) / "diagnostics",
        session_id=session,
    )


def _assert_pixel_exact(test: unittest.TestCase, left: object, right: object) -> None:
    test.assertTrue(np.array_equal(left.styled_rgb, right.styled_rgb))
    test.assertTrue(np.array_equal(left.levels, right.levels))
    test.assertTrue(np.array_equal(left.outline, right.outline))
    test.assertTrue(np.array_equal(left.coverage, right.coverage))
    test.assertTrue(np.array_equal(left.brush_rgba[..., 3], right.brush_rgba[..., 3]))


class ParallelAndRecoveryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene = sample_scene("orb_and_crate")

    def test_parallel_and_serial_are_pixel_and_commit_identical(self) -> None:
        with tempfile.TemporaryDirectory() as left_dir, tempfile.TemporaryDirectory() as right_dir:
            serial_engine = _engine(
                self.scene, fast_params(workers=1), left_dir, "serial"
            )
            parallel_engine = _engine(
                self.scene, fast_params(workers=4), right_dir, "parallel"
            )
            serial = serial_engine.render_exact(check_preview=False)
            parallel = parallel_engine.render_exact(check_preview=False)
            self.assertTrue(serial.complete)
            self.assertTrue(parallel.complete)
            self.assertIsNotNone(serial.frame)
            self.assertIsNotNone(parallel.frame)
            _assert_pixel_exact(self, serial.frame, parallel.frame)
            self.assertEqual(serial.commit_order, parallel.commit_order)
            self.assertEqual(serial.commit_order, tuple(range(len(serial.commit_order))))

    def test_pause_resume_matches_one_shot(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            engine = _engine(self.scene, fast_params(workers=4), directory, "resume")
            preview = engine.render_preview()
            first = engine.render_exact(
                preview=preview,
                control=RenderControl(pause_after_commits=3),
            )
            self.assertFalse(first.complete)
            self.assertTrue(first.paused)
            self.assertEqual(first.commit_order, (0, 1, 2))
            resumed = engine.resume(first.snapshot)
            self.assertTrue(resumed.complete)
            one_shot_engine = _engine(
                self.scene, fast_params(workers=4), directory, "one-shot"
            )
            one_shot = one_shot_engine.render_one_shot(snapshot=first.snapshot)
            self.assertIsNotNone(resumed.frame)
            _assert_pixel_exact(self, resumed.frame, one_shot)
            self.assertEqual(
                resumed.commit_order,
                tuple(range(len(resumed.commit_order))),
            )

    def test_new_engine_restart_resumes_committed_window(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            params = fast_params(workers=4)
            first_engine = _engine(self.scene, params, directory, "restart")
            preview = first_engine.render_preview()
            paused = first_engine.render_exact(
                preview=preview,
                control=RenderControl(pause_after_commits=4),
            )
            self.assertFalse(paused.complete)
            self.assertEqual(paused.commit_order, (0, 1, 2, 3))

            restarted_engine = _engine(self.scene, params, directory, "restart")
            restarted = restarted_engine.render_exact(check_preview=False)
            self.assertTrue(restarted.complete)
            self.assertEqual(
                restarted.commit_order,
                tuple(range(len(restarted.commit_order))),
            )
            one_shot_engine = _engine(
                self.scene, params, directory, "restart-one-shot"
            )
            one_shot = one_shot_engine.render_one_shot(snapshot=paused.snapshot)
            self.assertIsNotNone(restarted.frame)
            _assert_pixel_exact(self, restarted.frame, one_shot)

    def test_version_mismatch_invalidates_whole_checkpoint(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            params = fast_params(workers=4, levels=5)
            engine = _engine(self.scene, params, directory, "invalidate")
            preview = engine.render_preview()
            first = engine.render_exact(
                preview=preview,
                control=RenderControl(pause_after_commits=2),
            )
            self.assertFalse(first.complete)
            new_snapshot = engine.store.update(
                params=params.with_controls(levels=6)
            )
            engine.render_preview()
            resumed = engine.resume(new_snapshot)
            self.assertTrue(resumed.complete)
            self.assertTrue(resumed.checkpoint_invalidated)
            self.assertIn("mismatch", resumed.invalid_reason or "")
            self.assertIsNotNone(resumed.frame)
            self.assertLessEqual(int(np.max(resumed.frame.levels)), 5)
            session_dir = engine.checkpoint_root / "session"
            self.assertTrue((session_dir / "invalidated.json").exists())
            manifest = json.loads((session_dir / "manifest.json").read_text(encoding="utf-8"))
            self.assertEqual(manifest["identity"]["version"], new_snapshot.version)
            self.assertEqual(len(manifest["blocks"]), len(manifest["commit_order"]))

    def test_failed_tile_retry_does_not_pollute_other_blocks(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            params = fast_params(workers=4)
            engine = _engine(self.scene, params, directory, "fault")
            preview = engine.render_preview()
            fault = FaultInjector({3})
            failed_run = engine.render_exact(
                preview=preview,
                fault_injector=fault,
            )
            self.assertFalse(failed_run.complete)
            self.assertEqual(failed_run.failed_tiles, (3,))
            self.assertNotIn(3, failed_run.committed_tiles)
            self.assertEqual(len(failed_run.committed_tiles), failed_run.total_tiles - 1)
            session_dir = engine.checkpoint_root / "session"
            manifest_before = json.loads(
                (session_dir / "manifest.json").read_text(encoding="utf-8")
            )
            block_zero_before = manifest_before["blocks"]["0"]["sha256"]

            fault.clear()
            retried = engine.resume(failed_run.snapshot)
            self.assertTrue(retried.complete)
            self.assertEqual(retried.failed_tiles, ())
            self.assertEqual(retried.commit_order[-1], 3)
            manifest_after = json.loads(
                (session_dir / "manifest.json").read_text(encoding="utf-8")
            )
            self.assertEqual(manifest_after["blocks"]["0"]["sha256"], block_zero_before)

            one_shot_engine = _engine(
                self.scene, params, directory, "fault-one-shot"
            )
            one_shot = one_shot_engine.render_one_shot(snapshot=failed_run.snapshot)
            self.assertIsNotNone(retried.frame)
            _assert_pixel_exact(self, retried.frame, one_shot)

    def test_unexpected_tile_exception_is_isolated_and_retryable(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            params = fast_params(workers=4)
            engine = _engine(self.scene, params, directory, "generic-fault")
            preview = engine.render_preview()
            original = engine_module.render_tile

            def flaky(*args: object, **kwargs: object) -> object:
                tile = args[4]
                if tile.tile_id == 5:  # type: ignore[attr-defined]
                    raise RuntimeError("unexpected local tile exception")
                return original(*args, **kwargs)

            with patch.object(engine_module, "render_tile", side_effect=flaky):
                failed_run = engine.render_exact(preview=preview)
            self.assertFalse(failed_run.complete)
            self.assertEqual(failed_run.failed_tiles, (5,))
            self.assertNotIn(5, failed_run.committed_tiles)

            retried = engine.resume(failed_run.snapshot)
            self.assertTrue(retried.complete)
            self.assertIsNotNone(retried.frame)


if __name__ == "__main__":
    unittest.main()

