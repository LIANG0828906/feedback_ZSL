from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from spectraforge.checkpoint import CheckpointManager
from spectraforge.errors import WorkbenchError
from spectraforge.metadata import load_scene
from spectraforge.pipeline import assemble_output
from spectraforge.scheduler import ProcessingJob, ResourceConfig
from spectraforge.segmentation import make_segments, segmentation_spec
from spectraforge.state import SegmentRecord, SegmentState, SegmentStateMachine

ROOT = Path(__file__).resolve().parents[1]
SCENE_PATH = ROOT / "samples" / "scenes" / "neon_forge_424242"


def _setup(workspace: Path):
    scene = load_scene(SCENE_PATH)
    segments = make_segments(
        scene.width,
        scene.height,
        scene.band_count,
        tile_width=64,
        tile_height=64,
    )
    config = segmentation_spec(
        scene.width,
        scene.height,
        scene.band_count,
        split_mode="tiles",
        tile_width=64,
        tile_height=64,
        interpolation="bilinear",
    )
    return scene, segments, config


class TestStateAndCheckpoint(unittest.TestCase):
    def test_illegal_transition_preserves_state(self) -> None:
        records = {0: SegmentRecord(0), 1: SegmentRecord(1)}
        machine = SegmentStateMachine(records)
        with self.assertRaises(WorkbenchError) as raised:
            machine.transition(0, SegmentState.COMMITTED)
        self.assertEqual(raised.exception.code, "E_STATE")
        self.assertEqual(records[0].state, SegmentState.PENDING)
        machine.transition(0, SegmentState.PROCESSING)
        machine.transition(0, SegmentState.COMMITTED)
        with self.assertRaises(WorkbenchError):
            machine.transition(0, SegmentState.FAILED)
        self.assertEqual(records[0].state, SegmentState.COMMITTED)

    def test_committed_parameter_buffers_are_locked(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-params-") as temp:
            scene, segments, config = _setup(Path(temp))
            manager = CheckpointManager(Path(temp), scene, segments, config)
            try:
                params1 = scene.metadata.default_parameters
                job1 = ProcessingJob(
                    scene,
                    manager,
                    resource=ResourceConfig(1, 64),
                    params=params1,
                )
                job1.run(pause_after=1)
                committed = [
                    index
                    for index, record in manager.state.records.items()
                    if record.state == SegmentState.COMMITTED
                ]
                self.assertTrue(committed)
                before = {
                    index: (manager.snapshot_dir / manager.state.records[index].file).read_bytes()
                    for index in committed
                }
                versions = {
                    index: manager.load_segment(index)["meta"]["param_version"]
                    for index in committed
                }
                params2 = dict(params1)
                params2["weight_power"] = 4.0
                params2["gamut_intent"] = "clip"
                params2["tone_key"] = 1.4
                job2 = ProcessingJob(
                    scene,
                    manager,
                    resource=ResourceConfig(1, 64),
                    params=params2,
                )
                result = job2.run()
                self.assertTrue(result["ok"])
                for index in committed:
                    after = (manager.snapshot_dir / manager.state.records[index].file).read_bytes()
                    self.assertEqual(before[index], after)
                    self.assertEqual(versions[index], manager.load_segment(index)["meta"]["param_version"])
                self.assertGreaterEqual(len(manager.manifest["parameter_snapshots"]), 2)
            finally:
                manager.close()

    def test_one_corrupt_segment_invalidates_whole_snapshot(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-corrupt-") as temp:
            scene, segments, config = _setup(Path(temp))
            manager = CheckpointManager(Path(temp), scene, segments, config)
            job = ProcessingJob(scene, manager, resource=ResourceConfig(2, 64), params=scene.metadata.default_parameters)
            self.assertTrue(job.run()["ok"])
            snapshot_dir = manager.snapshot_dir
            manager.close()
            segment_file = next((snapshot_dir / "segments").glob("*.npz"))
            corrupt = bytearray(segment_file.read_bytes())
            corrupt[-8] ^= 0xAA
            segment_file.write_bytes(corrupt)
            reopened = CheckpointManager(Path(temp), scene, segments, config)
            try:
                self.assertIsNotNone(reopened.invalidated_reason)
                self.assertEqual(reopened.progress()["committed"], 0)
                self.assertEqual(reopened.progress()["pending"], len(segments))
                archives = list((Path(temp) / "checkpoints" / "_invalid").glob("*"))
                self.assertEqual(len(archives), 1)
            finally:
                reopened.close()

    def test_one_failed_segment_can_be_retried_without_polluting_others(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-fail-") as temp:
            scene, segments, config = _setup(Path(temp))
            manager = CheckpointManager(Path(temp), scene, segments, config)
            try:
                job = ProcessingJob(
                    scene,
                    manager,
                    resource=ResourceConfig(2, 64),
                    params=scene.metadata.default_parameters,
                )
                result = job.run(fail_segment=1)
                self.assertFalse(result["ok"])
                self.assertEqual(result["progress"]["failed"], 1)
                self.assertEqual(result["progress"]["committed"], len(segments) - 1)
                partial = assemble_output(scene, manager, require_complete=False)
                self.assertFalse(partial.complete)
                with self.assertRaises(WorkbenchError) as incomplete:
                    assemble_output(scene, manager, require_complete=True)
                self.assertEqual(incomplete.exception.code, "E_INCOMPLETE")
                retry = ProcessingJob(
                    scene,
                    manager,
                    resource=ResourceConfig(2, 64),
                    params=scene.metadata.default_parameters,
                )
                final = retry.run(retry_failed=True)
                self.assertTrue(final["ok"])
                self.assertEqual(final["progress"]["committed"], len(segments))
            finally:
                manager.close()

    def test_strict_gamut_failure_reports_global_pixel_and_channel(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-gamut-") as temp:
            scene, segments, config = _setup(Path(temp))
            manager = CheckpointManager(Path(temp), scene, segments, config)
            try:
                params = dict(scene.metadata.default_parameters)
                params["gamut_intent"] = "strict"
                job = ProcessingJob(
                    scene,
                    manager,
                    resource=ResourceConfig(2, 64),
                    params=params,
                )
                result = job.run()
                self.assertFalse(result["ok"])
                failed = [
                    record
                    for record in manager.state.records.values()
                    if record.state == SegmentState.FAILED
                ]
                self.assertTrue(failed)
                error = failed[0].error
                self.assertEqual(error["code"], "E_GAMUT")
                self.assertIn("x", error["details"])
                self.assertIn("y", error["details"])
                self.assertIn(error["details"]["channel"], {"R", "G", "B"})
            finally:
                manager.close()

    def test_forced_termination_recovers_only_committed_segments(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-crash-") as temp:
            script = f"""
from pathlib import Path
from spectraforge.metadata import load_scene
from spectraforge.segmentation import make_segments, segmentation_spec
from spectraforge.checkpoint import CheckpointManager
from spectraforge.scheduler import ProcessingJob, ResourceConfig
scene = load_scene({str(SCENE_PATH)!r})
segments = make_segments(scene.width, scene.height, scene.band_count, tile_width=64, tile_height=64)
config = segmentation_spec(scene.width, scene.height, scene.band_count, split_mode='tiles',
                           tile_width=64, tile_height=64, interpolation='bilinear')
manager = CheckpointManager(Path({temp!r}), scene, segments, config)
job = ProcessingJob(scene, manager, resource=ResourceConfig(1, 64), params=scene.metadata.default_parameters)
job.run(crash_after=2)
"""
            completed = subprocess.run(
                [sys.executable, "-c", script],
                cwd=ROOT,
                capture_output=True,
                text=True,
                timeout=30,
            )
            self.assertEqual(completed.returncode, 86, completed.stderr)
            scene, segments, config = _setup(Path(temp))
            manager = CheckpointManager(Path(temp), scene, segments, config)
            try:
                progress = manager.progress()
                self.assertEqual(progress["committed"], 2)
                self.assertEqual(progress["pending"], len(segments) - 2)
                with self.assertRaises(WorkbenchError) as incomplete:
                    assemble_output(scene, manager, require_complete=True)
                self.assertEqual(incomplete.exception.code, "E_INCOMPLETE")
                job = ProcessingJob(
                    scene,
                    manager,
                    resource=ResourceConfig(2, 64),
                    params=scene.metadata.default_parameters,
                )
                result = job.run()
                self.assertTrue(result["ok"])
                output = assemble_output(scene, manager, require_complete=True)
                self.assertTrue(output.complete)
            finally:
                manager.close()


if __name__ == "__main__":
    unittest.main()
