from __future__ import annotations

import copy
import tempfile
import unittest
from pathlib import Path

import numpy as np

from audio_sync.checkpoint import CheckpointStore, SegmentCheckpoint
from audio_sync.config import AlgorithmInfo, AlignmentConfig
from audio_sync.engine import AlignmentEngine, compare_results, run_three_paths
from audio_sync.errors import E_DIFF, E_STATE, SyncError
from audio_sync.generator import SyntheticSessionConfig, generate_session
from audio_sync.manifest import load_session
from audio_sync.state import SegmentState, SegmentStateMachine


class ThreePathAndCheckpointTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="audio-sync-paths-")
        cls.root = Path(cls.temp.name)
        session_dir = cls.root / "session"
        generate_session(
            session_dir,
            config=SyntheticSessionConfig(session_id="path-test", seed=20261124, duration_seconds=7.0),
        )
        cls.session = load_session(session_dir)
        cls.config = AlignmentConfig()
        cls.verification = run_three_paths(
            cls.session,
            cls.config,
            checkpoint_root=cls.root / "three-path",
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_three_paths_are_sample_identical(self) -> None:
        self.assertTrue(all(item["ok"] for item in self.verification.comparisons))
        for channel_id in self.verification.full.channels:
            self.assertTrue(
                np.array_equal(
                    self.verification.full.channels[channel_id],
                    self.verification.realtime.channels[channel_id],
                )
            )
            self.assertTrue(
                np.array_equal(
                    self.verification.full.channels[channel_id],
                    self.verification.resumed.channels[channel_id],
                )
            )
        self.assertGreater(self.verification.resumed.diagnostics["reused_committed_segments"], 0)

    def test_compare_reports_first_mismatch_location(self) -> None:
        candidate = copy.copy(self.verification.full)
        candidate.channels = {
            channel: np.array(data, copy=True)
            for channel, data in self.verification.full.channels.items()
        }
        candidate.channels["capture_a"][1234] += 1.0e-9
        with self.assertRaises(SyncError) as context:
            compare_results(self.verification.full, candidate, label="mutated")
        error = context.exception
        self.assertEqual(error.code, E_DIFF)
        self.assertEqual(error.channel, "capture_a")
        self.assertEqual(error.sample, 1234)
        self.assertEqual(error.segment, 0)

    def test_state_machine_rejects_illegal_transition_and_preserves_state(self) -> None:
        machine = SegmentStateMachine("capture_a", 2)
        with self.assertRaises(SyncError) as context:
            machine.transition(SegmentState.COMMITTED)
        self.assertEqual(context.exception.code, E_STATE)
        self.assertEqual(machine.state, SegmentState.PENDING)
        machine.transition(SegmentState.CORRECTED)
        machine.transition(SegmentState.COMMITTED)
        with self.assertRaises(SyncError):
            machine.transition(SegmentState.PENDING)
        self.assertEqual(machine.state, SegmentState.COMMITTED)

    def test_checkpoint_version_mismatch_invalidates_whole_snapshot(self) -> None:
        path = self.root / "versioned.sqlite"
        first = CheckpointStore(path, AlgorithmInfo(version="1.0.0"))
        status, grouped = first.load_snapshot("snapshot-test")
        self.assertFalse(status.invalidated)
        first.commit_segment(
            "snapshot-test",
            SegmentCheckpoint(
                channel_id="capture_a",
                segment_index=0,
                start=0,
                stop=10,
                params_json="{}",
                content_hash="content",
                params_version="p1",
                algorithm_version="1.0.0",
                output=np.arange(10, dtype=np.float64),
                output_hash="",
            ),
        )
        first.close()

        second = CheckpointStore(path, AlgorithmInfo(version="2.0.0"))
        status, grouped = second.load_snapshot("snapshot-test")
        self.assertTrue(status.invalidated)
        self.assertEqual(status.committed_count, 0)
        self.assertEqual(grouped, {})
        second.commit_segment(
            "snapshot-test",
            SegmentCheckpoint(
                channel_id="capture_a",
                segment_index=0,
                start=0,
                stop=10,
                params_json="{}",
                content_hash="content",
                params_version="p2",
                algorithm_version="2.0.0",
                output=np.arange(10, dtype=np.float64),
                output_hash="",
            ),
        )
        second.close()

        third = CheckpointStore(path, AlgorithmInfo(version="2.0.0"))
        status, grouped = third.load_snapshot("snapshot-test")
        self.assertFalse(status.invalidated)
        self.assertEqual(status.committed_count, 1)
        self.assertEqual(len(grouped["capture_a"]), 1)
        third.close()

    def test_parameter_change_preserves_committed_and_recomputes_rest(self) -> None:
        checkpoint = self.root / "preserve.sqlite"
        base = AlignmentConfig(preserve_committed=True)
        prefix = AlignmentEngine(
            self.session,
            base,
            checkpoint_path=checkpoint,
        ).run("realtime", interrupt_after=2)
        self.assertFalse(prefix.complete)

        changed = AlignmentConfig(interpolation_order=5, preserve_committed=True)
        resumed = AlignmentEngine(
            self.session,
            changed,
            checkpoint_path=checkpoint,
        ).run("resume")
        boundary = int(prefix.segment_boundaries[2])
        self.assertEqual(resumed.diagnostics["reused_committed_segments"], 8)
        for channel_id in self.session.manifest.channel_ids():
            self.assertTrue(
                np.array_equal(
                    prefix.channels[channel_id][:boundary],
                    resumed.channels[channel_id][:boundary],
                )
            )
        self.assertGreater(resumed.diagnostics["newly_processed_segments"], 0)


if __name__ == "__main__":
    unittest.main()

