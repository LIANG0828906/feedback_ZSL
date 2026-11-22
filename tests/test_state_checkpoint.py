from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from mshdr.dataset import DatasetIndex
from mshdr.errors import WorkbenchError
from mshdr.params import WorkbenchParams
from mshdr.pipeline import WorkbenchEngine
from mshdr.segments import SegmentState, SegmentStateMachine, build_segments

from support import generate_single_scene


class StateCheckpointTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.index = generate_single_scene(cls.root)
        cls.scene = cls.index.load_scene(cls.index.scene_records[0]["scene_id"])
        cls.params = WorkbenchParams(max_workers=2)
        reference_engine = WorkbenchEngine(cls.scene, cls.params)
        reference_engine.run(mode="reference")
        cls.reference = reference_engine.assemble()

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_state_machine_legal_and_illegal_transitions(self) -> None:
        segments = build_segments(*self.scene.pixel_size, (32, 32))
        machine = SegmentStateMachine(segments)
        self.assertEqual(machine.progress, f"0/{len(segments)}")
        machine.begin(0)
        self.assertEqual(machine.records[0].state, SegmentState.PROCESSING)
        machine.mark_committed(
            0,
            metadata_snapshot={"metadata_version": self.scene.metadata_version},
            params_snapshot=self.params.algorithm_snapshot(),
            metadata_version=self.scene.metadata_version,
            params_version=self.params.version(),
            algorithm_version="test-algo",
        )
        self.assertEqual(machine.committed_count, 1)
        with self.assertRaises(WorkbenchError) as context:
            machine.transition(0, SegmentState.PROCESSING)
        self.assertEqual(context.exception.code, "E_STATE")
        self.assertEqual(machine.records[0].state, SegmentState.COMMITTED)
        self.assertEqual(machine.records[0].algorithm_version, "test-algo")

    def test_live_parameters_do_not_rewrite_committed_segments(self) -> None:
        engine = WorkbenchEngine(self.scene, self.params)
        engine.run(commit_limit=3, mode="partial")
        original = {
            index: {name: array.copy() for name, array in payload.items()}
            for index, payload in engine.committed_arrays.items()
        }
        self.assertEqual(engine.committed_count, 3)
        changed_params = self.params.with_updates(contrast_gain=2.2)
        engine.update_params(changed_params)
        with tempfile.TemporaryDirectory() as directory:
            engine.run(output_dir=Path(directory) / "out", mode="live-update")
        self.assertEqual(engine.committed_count, engine.total_count)
        for index, arrays in original.items():
            for name, array in arrays.items():
                self.assertTrue(np.array_equal(array, engine.committed_arrays[index][name]), (index, name))
        self.assertTrue(any(
            engine.committed_arrays[index]["enhanced_detail"].shape == (32, 32)
            and engine.state_machine.records[index].params_version == changed_params.version()
            for index in range(3, engine.total_count)
        ))
        self.assertFalse(
            np.array_equal(engine.assemble()["enhanced_detail"], self.reference["enhanced_detail"])
        )

    def test_checkpoint_resume_is_pixel_identical(self) -> None:
        checkpoint_root = self.root / "checkpoints" / "resume_ok"
        first = WorkbenchEngine(
            self.scene,
            self.params,
            checkpoint_root=checkpoint_root,
            start_mode="fresh",
        )
        first.run(commit_limit=4, mode="first")
        resumed = WorkbenchEngine(
            self.scene,
            self.params,
            checkpoint_root=checkpoint_root,
            start_mode="resume",
        )
        self.assertEqual(resumed.committed_count, 4)
        with tempfile.TemporaryDirectory() as directory:
            resumed.run(output_dir=Path(directory) / "out", mode="resume")
        assembled = resumed.assemble()
        for name in assembled:
            self.assertTrue(np.array_equal(assembled[name], self.reference[name]), name)

    def test_one_corrupt_segment_invalidates_whole_checkpoint(self) -> None:
        checkpoint_root = self.root / "checkpoints" / "corrupt"
        first = WorkbenchEngine(
            self.scene,
            self.params,
            checkpoint_root=checkpoint_root,
            start_mode="fresh",
        )
        first.run(mode="first")
        segment_path = checkpoint_root / self.scene.scene_id / "segments" / "seg_00003.npz"
        data = bytearray(segment_path.read_bytes())
        data[len(data) // 2] ^= 0x7F
        segment_path.write_bytes(data)
        resumed = WorkbenchEngine(
            self.scene,
            self.params,
            checkpoint_root=checkpoint_root,
            start_mode="resume",
        )
        self.assertIsNotNone(resumed.checkpoint_invalid_reason)
        self.assertEqual(resumed.committed_count, 0)
        with tempfile.TemporaryDirectory() as directory:
            resumed.run(output_dir=Path(directory) / "out", mode="resume-recompute")
        assembled = resumed.assemble()
        for name in assembled:
            self.assertTrue(np.array_equal(assembled[name], self.reference[name]), name)

    def test_parameter_version_mismatch_invalidates_snapshot(self) -> None:
        checkpoint_root = self.root / "checkpoints" / "version"
        first = WorkbenchEngine(
            self.scene,
            self.params,
            checkpoint_root=checkpoint_root,
            start_mode="fresh",
        )
        first.run(commit_limit=2, mode="first")
        resumed = WorkbenchEngine(
            self.scene,
            self.params.with_updates(contrast_gain=1.9),
            checkpoint_root=checkpoint_root,
            start_mode="resume",
        )
        self.assertEqual(resumed.committed_count, 0)
        self.assertIsNotNone(resumed.checkpoint_invalid_reason)


if __name__ == "__main__":
    unittest.main()

