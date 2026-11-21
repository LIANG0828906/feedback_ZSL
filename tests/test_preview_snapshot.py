from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from volrender.buffers import BufferStore
from volrender.errors import ErrorCode, VolumeRenderError
from volrender.preview import PreviewController
from volrender.renderer import rebind_result, render_offscreen
from volrender.snapshot import (
    SnapshotManager,
    create_snapshot,
    load_snapshot,
    save_snapshot,
)
from tests.helpers import make_volume


class PreviewAlignmentTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.volume = make_volume(Path(cls._temp.name), size=20)
        from volrender.synthetic import baseline_parameters

        cls.params = baseline_parameters(cls.volume, viewport=(40, 40)).with_changes(step_size=0.04)
        cls.result = render_offscreen(cls.volume, cls.params)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_drag_keeps_committed_frame_and_marks_pending(self) -> None:
        controller = PreviewController()
        controller.commit(self.params, self.result)
        pending = self.params.with_changes(window_width=self.params.window_width * 1.2)
        controller.set_pending(pending)
        state = controller.state()
        self.assertEqual(state["displayed_parameter_hash"], self.params.parameter_hash)
        self.assertEqual(state["displayed_buffer_hash"], self.result.buffer_hash)
        self.assertEqual(state["pending_parameter_hash"], pending.parameter_hash)
        self.assertTrue(state["pending_is_uncommitted"])
        self.assertTrue(state["binding_valid"])
        self.assertEqual(controller.displayed_params, self.params)

    def test_mismatched_result_is_rejected_without_replacing_display(self) -> None:
        controller = PreviewController()
        controller.commit(self.params, self.result)
        pending = self.params.with_changes(window_width=self.params.window_width * 1.2)
        mismatched = rebind_result(self.result, pending.parameter_hash)
        with self.assertRaises(VolumeRenderError) as raised:
            controller.commit(pending, mismatched)
        self.assertEqual(raised.exception.code, ErrorCode.E_PREVIEW_MISMATCH)
        self.assertEqual(raised.exception.field, "window_width")
        self.assertEqual(controller.displayed_parameter_hash, self.params.parameter_hash)

    def test_commit_switches_only_after_new_result(self) -> None:
        controller = PreviewController()
        controller.commit(self.params, self.result)
        pending = self.params.with_changes(window_width=self.params.window_width * 1.3)
        controller.set_pending(pending)
        pending_result = render_offscreen(self.volume, pending)
        controller.commit(pending, pending_result)
        state = controller.state()
        self.assertEqual(state["displayed_parameter_hash"], pending.parameter_hash)
        self.assertIsNone(state["pending_parameter_hash"])
        self.assertFalse(state["pending_is_uncommitted"])


class SnapshotTests(unittest.TestCase):
    def setUp(self) -> None:
        self._temp = tempfile.TemporaryDirectory()
        self.root = Path(self._temp.name)
        self.volume = make_volume(self.root, size=20)
        from volrender.synthetic import baseline_parameters

        self.params = baseline_parameters(self.volume, viewport=(40, 40)).with_changes(step_size=0.04)
        self.result = render_offscreen(self.volume, self.params)
        self.store = BufferStore(self.root / "buffers")
        self.store.publish(self.result, self.params)
        self.snapshot = create_snapshot(self.params, self.result)

    def tearDown(self) -> None:
        self._temp.cleanup()

    def test_atomic_save_and_load(self) -> None:
        path = self.root / "snapshots" / "one.json"
        save_snapshot(self.snapshot, path)
        loaded = load_snapshot(path)
        self.assertEqual(loaded.parameter_hash, self.params.parameter_hash)
        self.assertEqual(loaded.color_buffer_hash, self.result.buffer_hash)
        self.assertEqual(loaded.viewport, self.params.viewport)
        self.assertFalse(any(path.parent.glob("*.tmp")))

    def test_missing_version_is_snapshot_invalid(self) -> None:
        path = self.root / "bad.json"
        save_snapshot(self.snapshot, path)
        payload = json.loads(path.read_text(encoding="utf-8"))
        del payload["version"]
        path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(VolumeRenderError) as raised:
            load_snapshot(path)
        self.assertEqual(raised.exception.code, ErrorCode.E_SNAPSHOT_INVALID)
        self.assertEqual(raised.exception.field, "version")

    def test_changed_top_level_field_is_snapshot_invalid(self) -> None:
        path = self.root / "bad-step.json"
        save_snapshot(self.snapshot, path)
        payload = json.loads(path.read_text(encoding="utf-8"))
        payload["step_size"] = 0.5
        path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(VolumeRenderError) as raised:
            load_snapshot(path)
        self.assertEqual(raised.exception.code, ErrorCode.E_SNAPSHOT_INVALID)
        self.assertEqual(raised.exception.field, "step_size")

    def test_restore_reuses_only_matching_verified_buffer(self) -> None:
        manager = SnapshotManager(self.store)
        path = self.root / "snapshots" / "restore.json"
        save_snapshot(self.snapshot, path)
        reused = manager.restore(self.snapshot, self.params, self.volume, path)
        self.assertTrue(reused.reused_buffer)
        self.assertFalse(reused.rerendered)
        self.assertEqual(reused.result.buffer_hash, self.result.buffer_hash)

        changed = self.params.with_changes(window_width=self.params.window_width * 1.25)
        rerendered = manager.restore(self.snapshot, changed, self.volume, path, save_after_rerender=False)
        self.assertFalse(rerendered.reused_buffer)
        self.assertTrue(rerendered.rerendered)
        self.assertIn("window_width", rerendered.changed_fields)
        self.assertEqual(rerendered.result.parameter_hash, changed.parameter_hash)
        self.assertNotEqual(rerendered.result.buffer_hash, self.result.buffer_hash)
        self.assertEqual(load_snapshot(path).parameter_hash, self.params.parameter_hash)

    def test_tampered_buffer_never_reused(self) -> None:
        manager = SnapshotManager(self.store)
        path = self.root / "snapshots" / "restore.json"
        save_snapshot(self.snapshot, path)
        manifest_path = self.root / "buffers" / f"{self.params.parameter_hash}.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["buffer_hash"] = "d" * 64
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        outcome = manager.restore(self.snapshot, self.params, self.volume, path)
        self.assertTrue(outcome.rerendered)
        self.assertFalse(outcome.reused_buffer)
        self.assertIn("color_buffer_hash", outcome.changed_fields)


if __name__ == "__main__":
    unittest.main()
