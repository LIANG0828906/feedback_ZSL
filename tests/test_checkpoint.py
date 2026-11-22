from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from tests.helpers import sample_path
from vp_pipeline.checkpoint import CheckpointStore
from vp_pipeline.loader import load_scene
from vp_pipeline.session import CompositorSession


class CheckpointTests(unittest.TestCase):
    def test_restart_restores_all_matching_windows(self) -> None:
        model = load_scene(sample_path(3))
        with tempfile.TemporaryDirectory() as directory:
            first = CompositorSession(model, CheckpointStore(directory))
            result = first.run(parallel=True)
            second = CompositorSession(model, CheckpointStore(directory))
            self.assertFalse(second.recovery.discarded)
            self.assertEqual(sorted(second.recovery.restored), [0, 1, 2, 3, 4, 5])
            self.assertEqual(second.progress, (6, 6))
            restored = second.current_frames()
            self.assertEqual(
                {index: frame.content_hash for index, frame in restored.items()},
                {index: frame.content_hash for index, frame in result.frames.items()},
            )

    def test_partial_persisted_windows_recover_after_termination(self) -> None:
        model = load_scene(sample_path(2))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory))

            def terminate_after_first_two(frame_index: int) -> None:
                if frame_index >= 6:
                    raise RuntimeError("simulated process termination")

            result = session.run(parallel=False, frame_hook=terminate_after_first_two)
            self.assertEqual(result.commit_order, (0, 1))
            self.assertEqual(result.failed_windows, (2, 3, 4, 5))
            restarted = CompositorSession(model, CheckpointStore(directory))
            self.assertFalse(restarted.recovery.discarded)
            self.assertEqual(sorted(restarted.recovery.restored), [0, 1])
            self.assertEqual(restarted.progress, (2, 6))

    def test_frame_hash_tamper_discards_entire_snapshot(self) -> None:
        model = load_scene(sample_path(3))
        with tempfile.TemporaryDirectory() as directory:
            first = CompositorSession(model, CheckpointStore(directory))
            first.run(parallel=False)
            frame_file = Path(directory) / "window_0000.bin"
            with frame_file.open("r+b") as handle:
                handle.seek(5)
                original = handle.read(1)
                handle.seek(5)
                handle.write(bytes([original[0] ^ 0xFF]))
            restarted = CompositorSession(model, CheckpointStore(directory))
            self.assertTrue(restarted.recovery.discarded)
            self.assertEqual(restarted.progress, (0, 6))
            self.assertEqual(restarted.recovery.restored, {})

    def test_algorithm_mismatch_discards_entire_snapshot(self) -> None:
        model = load_scene(sample_path(1))
        with tempfile.TemporaryDirectory() as directory:
            first = CompositorSession(model, CheckpointStore(directory))
            first.run(parallel=False)
            manifest_path = Path(directory) / "checkpoints.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            manifest["algorithm_version"] = "vp-compositor/0"
            manifest_path.write_text(
                json.dumps(manifest, sort_keys=True, indent=2) + "\n",
                encoding="utf-8",
            )
            restarted = CompositorSession(model, CheckpointStore(directory))
            self.assertTrue(restarted.recovery.discarded)
            self.assertEqual(restarted.progress, (0, 6))

    def test_source_version_mismatch_discards_entire_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            first = CompositorSession(
                load_scene(sample_path(3)), CheckpointStore(directory)
            )
            first.run(parallel=False)
            restarted = CompositorSession(
                load_scene(sample_path(1)), CheckpointStore(directory)
            )
            self.assertTrue(restarted.recovery.discarded)
            self.assertEqual(restarted.progress, (0, 6))

    def test_edited_model_recovers_only_after_all_snapshots_match(self) -> None:
        model = load_scene(sample_path(3))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory))
            session.run(parallel=False)
            session.apply_edit("prop_crate", "opacity", 0.33)
            partial = CompositorSession(
                session.model, CheckpointStore(directory)
            )
            self.assertTrue(partial.recovery.discarded)
            self.assertEqual(partial.progress, (0, 6))
            partial.run(parallel=False)
            recovered = CompositorSession(
                partial.model, CheckpointStore(directory)
            )
            self.assertFalse(recovered.recovery.discarded)
            self.assertEqual(recovered.progress, (6, 6))
            self.assertEqual(
                {
                    index: frame.content_hash
                    for index, frame in recovered.current_frames().items()
                },
                {
                    index: frame.content_hash
                    for index, frame in partial.current_frames().items()
                },
            )


if __name__ == "__main__":
    unittest.main()
