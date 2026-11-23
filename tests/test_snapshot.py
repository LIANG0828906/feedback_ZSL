from __future__ import annotations

import json
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from pathlib import Path

from gamepad_studio.concurrency import SerializedSnapshotWriter
from gamepad_studio.pipeline import Pipeline
from gamepad_studio.snapshot import SnapshotStore

from tests.helpers import load_bundle


class SnapshotTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="padweave-snapshot-")
        self.snapshot_path = Path(self.temporary.name) / "state.json"
        self.bundle = load_bundle("dual_stick")
        self.pipeline = Pipeline(self.bundle)
        self.original_run = self.pipeline.recompute()
        self.store = SnapshotStore(self.snapshot_path)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_valid_snapshot_restores_all_nodes_and_result(self) -> None:
        write = self.store.write(self.pipeline)
        self.assertEqual(write.node_count, len(self.pipeline.order))
        restored = Pipeline(self.bundle)
        load = self.store.load(restored)
        self.assertTrue(load.valid, load.to_dict())
        restored.install_cache_entries(load.entries)
        run = restored.recompute()
        self.assertEqual(run.recomputed_count, 0)
        self.assertEqual(run.reused_count, len(restored.order))
        self.assertEqual(run.result.result_hash, self.original_run.result.result_hash)

    def test_input_hash_mismatch_invalidates_whole_snapshot(self) -> None:
        self.store.write(self.pipeline)
        other = load_bundle("single_stick")
        load = self.store.load(Pipeline(other))
        self.assertFalse(load.valid)
        self.assertEqual(load.reason, "version_mismatch")
        self.assertIn("input_hash", load.version_mismatches)
        self.assertGreater(load.invalidated_node_count or 0, 0)
        self.assertEqual(load.entries, {})

    def test_binding_version_mismatch_invalidates_whole_snapshot(self) -> None:
        self.store.write(self.pipeline)
        bindings = self.bundle.bindings.with_rule("sprint", window_ms=120)
        load = self.store.load(
            Pipeline(replace(self.bundle, bindings=bindings))
        )
        self.assertFalse(load.valid)
        self.assertIn("binding_version", load.version_mismatches)
        self.assertEqual(load.entries, {})

    def test_algorithm_version_mismatch_invalidates_whole_snapshot(self) -> None:
        self.store.write(self.pipeline)
        payload = json.loads(self.snapshot_path.read_text(encoding="utf-8"))
        payload["metadata"]["algorithm_version"] = "0.0.0-test"
        self.snapshot_path.write_text(
            json.dumps(payload),
            encoding="utf-8",
        )
        load = self.store.load(Pipeline(self.bundle))
        self.assertFalse(load.valid)
        self.assertIn("algorithm_version", load.version_mismatches)
        self.assertEqual(load.entries, {})

    def test_corrupt_snapshot_is_rejected_without_partial_entries(self) -> None:
        self.snapshot_path.write_text("{broken", encoding="utf-8")
        load = self.store.load(Pipeline(self.bundle))
        self.assertFalse(load.valid)
        self.assertEqual(load.reason, "snapshot_corrupt")
        self.assertIsNone(load.invalidated_node_count)
        self.assertEqual(load.entries, {})

    def test_snapshot_replacement_reports_version_change_count(self) -> None:
        self.store.write(self.pipeline)
        changed_device = self.bundle.device.with_axis("left_x", deadzone=0.21)
        changed_pipeline = Pipeline(replace(self.bundle, device=changed_device))
        changed_pipeline.recompute()
        report = self.store.write(changed_pipeline)
        self.assertTrue(report.replaced)
        self.assertIn("graph_signature", report.version_changes)
        self.assertIn("param_version", report.version_changes)
        self.assertEqual(report.invalidated_node_count, len(self.pipeline.order))

    def test_serialized_snapshot_writes_do_not_interleave(self) -> None:
        writer = SerializedSnapshotWriter(self.store)
        with ThreadPoolExecutor(max_workers=4) as executor:
            reports = list(executor.map(lambda _: writer.write(self.pipeline), range(6)))
        self.assertEqual(writer.write_count, 6)
        self.assertEqual(len(reports), 6)
        restored = Pipeline(self.bundle)
        load = self.store.load(restored)
        self.assertTrue(load.valid)
        self.assertFalse(
            any(path.suffix == ".tmp" for path in Path(self.temporary.name).iterdir())
        )


if __name__ == "__main__":
    unittest.main()

