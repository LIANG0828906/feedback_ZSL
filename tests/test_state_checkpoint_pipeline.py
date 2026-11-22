from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from artmask.checkpoint import CheckpointIdentity, CheckpointStore
from artmask.config import ALGORITHM_VERSION, Budget, SegmentationParams
from artmask.errors import BusinessError
from artmask.pipeline import SegmentationPipeline, export_artifacts
from artmask.samplegen import DEFAULT_SAMPLES, generate_suite
from artmask.state import (
    BlockGrid,
    BlockStateMachine,
    Stage,
    StageState,
)
from artmask.validation import array_hash, load_sample


class StateMachineTests(unittest.TestCase):
    def test_legal_and_illegal_transitions_preserve_state(self) -> None:
        state = BlockStateMachine(BlockGrid(64, 64, 64))
        with self.assertRaises(BusinessError) as context:
            state.transition(0, Stage.GROWTH, StageState.COMMITTED)
        self.assertEqual(context.exception.code, "E_STATE")
        self.assertEqual(state.state(0, Stage.GROWTH), StageState.PENDING)
        state.begin(0, Stage.GROWTH, "t1")
        state.commit(0, Stage.GROWTH, b"x", "hash", sequence=1)
        self.assertEqual(state.state(0, Stage.GROWTH), StageState.COMMITTED)
        with self.assertRaises(BusinessError) as context:
            state.transition(0, Stage.GROWTH, StageState.PROCESSING)
        self.assertEqual(context.exception.code, "E_STATE")
        self.assertEqual(state.state(0, Stage.GROWTH), StageState.COMMITTED)

    def test_stage_order_is_enforced(self) -> None:
        state = BlockStateMachine(BlockGrid(64, 64, 64))
        with self.assertRaises(BusinessError) as context:
            state.begin(0, Stage.CLEANUP, "t1")
        self.assertEqual(context.exception.code, "E_STATE")
        self.assertEqual(state.state(0, Stage.CLEANUP), StageState.PENDING)

    def test_failed_stage_can_retry_and_committed_is_frozen(self) -> None:
        state = BlockStateMachine(BlockGrid(64, 64, 64))
        state.begin(0, Stage.GROWTH, "t1")
        state.fail(0, Stage.GROWTH, "boom")
        self.assertEqual(state.state(0, Stage.GROWTH), StageState.FAILED)
        state.begin(0, Stage.GROWTH, "t2")
        self.assertEqual(state.state(0, Stage.GROWTH), StageState.PROCESSING)
        state.commit(0, Stage.GROWTH, b"payload", "hash", sequence=1)
        with self.assertRaises(BusinessError):
            state.commit(0, Stage.GROWTH, b"other", "hash2", sequence=2)

    def test_invalidation_lists_only_uncommitted_blocks(self) -> None:
        state = BlockStateMachine(BlockGrid(80, 64, 64))
        state.begin(0, Stage.GROWTH, "t1")
        state.commit(0, Stage.GROWTH, b"x", "hash", sequence=1)
        invalidated = state.invalidate_uncommitted()
        self.assertEqual(invalidated, [0, 1])
        self.assertEqual(state.state(0, Stage.GROWTH), StageState.COMMITTED)


class PipelineCheckpointTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.sample_root = Path(cls.temp.name) / "samples"
        generate_suite(cls.sample_root, (DEFAULT_SAMPLES[0],))
        cls.sample = load_sample(cls.sample_root / DEFAULT_SAMPLES[0].sample_id)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _workspace(self, name: str) -> Path:
        path = Path(self.temp.name) / name
        if path.exists():
            shutil.rmtree(path)
        path.mkdir(parents=True)
        return path

    def _params(self) -> SegmentationParams:
        return SegmentationParams().for_sample(
            self.sample.recommended_params, self.sample.region_count
        )

    def _budget(self) -> Budget:
        return Budget(
            time_seconds=5.0,
            memory_mb=128.0,
            max_workers=4,
            tile_size=256,
            block_buffer_mb=4.0,
        )

    def test_complete_restart_reuses_version_matched_snapshot(self) -> None:
        root = self._workspace("complete-recovery")
        pipeline = SegmentationPipeline(root / "checkpoints")
        first = pipeline.run(self.sample, self._params(), self._budget())
        self.assertTrue(first.complete)
        second = pipeline.run(self.sample, self._params(), self._budget())
        self.assertTrue(second.complete)
        self.assertTrue(second.reused_snapshot)
        self.assertEqual(second.recovered_stages, second.state_snapshot["total_stages"])
        self.assertTrue(np.array_equal(first.final_labels, second.final_labels))

    def test_failure_is_isolated_and_retry_resumes_committed_stages(self) -> None:
        root = self._workspace("failure-retry")
        pipeline = SegmentationPipeline(root / "checkpoints")
        first = pipeline.run(
            self.sample,
            self._params(),
            self._budget(),
            fail_injections={(0, Stage.CLEANUP)},
        )
        self.assertFalse(first.complete)
        self.assertEqual(
            first.state_snapshot["blocks"][0]["stages"]["cleanup"]["state"],
            "failed",
        )
        self.assertEqual(
            first.state_snapshot["blocks"][1]["stages"]["statistics"]["state"],
            "committed",
        )
        second = pipeline.run(self.sample, self._params(), self._budget())
        self.assertTrue(second.complete)
        self.assertGreater(second.recovered_stages, 0)
        self.assertTrue(np.array_equal(first.final_labels, second.final_labels))

    def test_parameter_version_mismatch_invalidates_entire_snapshot(self) -> None:
        root = self._workspace("parameter-mismatch")
        pipeline = SegmentationPipeline(root / "checkpoints")
        pipeline.run(self.sample, self._params(), self._budget())
        changed = SegmentationParams(
            **{
                **self._params().__dict__,
                "color_tolerance": self._params().color_tolerance + 1.0,
            }
        )
        second = pipeline.run(self.sample, changed, self._budget())
        self.assertTrue(second.checkpoint_invalidated)
        self.assertFalse(second.reused_snapshot)
        self.assertEqual(second.recovered_stages, 0)
        self.assertTrue(second.complete)

    def test_image_hash_mismatch_invalidates_snapshot(self) -> None:
        root = self._workspace("image-mismatch")
        pipeline = SegmentationPipeline(root / "checkpoints")
        pipeline.run(self.sample, self._params(), self._budget())
        changed_image = self.sample.image.copy()
        changed_image[0, 0] = np.array([1, 2, 3], dtype=np.uint8)
        original = self.sample.image
        self.sample.image = changed_image
        try:
            second = pipeline.run(self.sample, self._params(), self._budget())
        finally:
            self.sample.image = original
        self.assertTrue(second.checkpoint_invalidated)
        self.assertGreaterEqual(
            second.recovery_reason.find("image_hash"),
            0,
        )

    def test_checkpoint_records_are_atomic_hash_checked_and_ordered(self) -> None:
        root = self._workspace("atomic-records")
        pipeline = SegmentationPipeline(root / "checkpoints")
        result = pipeline.run(self.sample, self._params(), self._budget())
        snapshot = Path(result.checkpoint_dir)
        files = sorted(snapshot.glob("b*_*.npz"))
        self.assertEqual(len(files), result.state_snapshot["total_stages"])
        sequences = []
        for path in files:
            with np.load(path, allow_pickle=False) as data:
                payload = np.asarray(data["payload"], dtype=np.uint8).tobytes()
                expected_hash = str(data["payload_hash"].item())
                import hashlib

                self.assertEqual(hashlib.sha256(payload).hexdigest(), expected_hash)
                sequences.append(int(data["sequence"].item()))
        self.assertEqual(sorted(sequences), list(range(1, len(files) + 1)))
        index = json.loads((snapshot / "index.json").read_text(encoding="utf-8"))
        self.assertEqual(index["count"], len(files))
        self.assertEqual(
            [record["sequence"] for record in index["records"]],
            list(range(1, len(files) + 1)),
        )

    def test_block_buffer_limit_returns_e_para(self) -> None:
        root = self._workspace("buffer-limit")
        identity = CheckpointIdentity(
            sample_id="buffer",
            image_hash=array_hash(self.sample.image),
            parameter_version="p",
            algorithm_version=ALGORITHM_VERSION,
            budget_version="b",
            mode="fine",
            factor=1,
            block_size=256,
            block_count=1,
        )
        store = CheckpointStore(root, identity, block_buffer_mb=0.001)
        store.open()
        with self.assertRaises(BusinessError) as context:
            store.commit(0, Stage.GROWTH, b"x" * 4096)
        self.assertEqual(context.exception.code, "E_PARA")
        self.assertEqual(context.exception.field, "budget.block_buffer_mb")

    def test_exports_mask_and_report(self) -> None:
        root = self._workspace("exports")
        pipeline = SegmentationPipeline(root / "checkpoints")
        result = pipeline.run(self.sample, self._params(), self._budget())
        paths = export_artifacts(result, root / "exports", image=self.sample.image)
        for path in paths.values():
            self.assertTrue(Path(path).is_file())
        mask = np.load(paths["mask"], allow_pickle=False)
        self.assertTrue(np.array_equal(mask, result.final_labels))


if __name__ == "__main__":
    unittest.main()
