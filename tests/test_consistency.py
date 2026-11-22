from __future__ import annotations

import copy
import tempfile
import unittest
from pathlib import Path

from scworkbench.compare import compare_runs
from scworkbench.engine import EngineConfig
from scworkbench.loader import parse_session
from scworkbench.runner import execute_paths

from tests.helpers import sample_data


class ConsistencyTests(unittest.TestCase):
    def test_three_paths_are_frame_and_event_identical(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            execution = execute_paths(
                sample,
                EngineConfig(checkpoint_every=5),
                Path(temp) / "checkpoints",
                recovery_after_frames=18,
            )
        self.assertTrue(execution.comparison.ok, execution.comparison.to_dict())
        self.assertEqual(execution.recovery_checkpoint.status, "loaded")
        for result in (execution.realtime, execution.recovery, execution.replay):
            self.assertEqual(len(result.frames), 40)
            self.assertEqual(result.final_phase, "disconnected")
            self.assertEqual(result.stats.frames_committed, 40)
            self.assertEqual(result.stats.inputs_replayed, 27)
            self.assertEqual(result.desktop_state["revision"], 27)
        self.assertEqual(
            [item.frame_sha256 for item in execution.realtime.frames],
            [item.frame_sha256 for item in execution.recovery.frames],
        )
        self.assertEqual(
            [item.desktop_digest for item in execution.realtime.inputs],
            [item.desktop_digest for item in execution.replay.inputs],
        )

    def test_frame_diff_locates_first_index_field_and_phase(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            execution = execute_paths(
                sample,
                EngineConfig(checkpoint_every=5),
                Path(temp) / "checkpoints",
                recovery_after_frames=18,
            )
        damaged = copy.deepcopy(execution.replay)
        damaged.frames[17].frame_sha256 = "0" * 64
        report = compare_runs(execution.realtime, damaged)
        self.assertFalse(report.ok)
        self.assertEqual(report.code, "E_DIFF")
        self.assertEqual(report.component, "frame")
        self.assertEqual(report.index, 18)
        self.assertEqual(report.field, "frame_sha256")
        self.assertEqual(report.phase, "mirroring")

    def test_input_diff_locates_per_event_state(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            execution = execute_paths(
                sample,
                EngineConfig(checkpoint_every=5),
                Path(temp) / "checkpoints",
                recovery_after_frames=18,
            )
        damaged = copy.deepcopy(execution.replay)
        damaged.frames[17].frame_sha256 = "0" * 64
        damaged.inputs[8].desktop_digest = "f" * 64
        report = compare_runs(execution.realtime, damaged)
        self.assertFalse(report.ok)
        self.assertEqual(report.code, "E_DIFF")
        self.assertEqual(report.component, "input")
        self.assertEqual(report.index, 9)
        self.assertEqual(report.field, "desktop_digest")

    def test_mixed_faults_still_match_across_paths(self) -> None:
        sample = parse_session(sample_data())
        from scworkbench.anomalies import default_presets

        with tempfile.TemporaryDirectory() as temp:
            execution = execute_paths(
                sample,
                EngineConfig(
                    checkpoint_every=5,
                    anomaly=default_presets()["mixed"],
                ),
                Path(temp) / "checkpoints",
                recovery_after_frames=18,
            )
        self.assertTrue(execution.comparison.ok, execution.comparison.to_dict())
        self.assertEqual(execution.realtime.stats.frames_committed, 40)
        self.assertGreater(execution.realtime.stats.frames_missing, 0)


if __name__ == "__main__":
    unittest.main()
