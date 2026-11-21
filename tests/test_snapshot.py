from __future__ import annotations

import json
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from cover_replay.engine import ReplayEngine
from cover_replay.errors import BusinessError, RecoveryInvalidError
from cover_replay.snapshot import SnapshotStore
from cover_replay.util import atomic_write_json, load_json

from tests.common import config, cover, record, shot


class SnapshotTests(unittest.TestCase):
    def setUp(self) -> None:
        self.config = config(physics_steps=6)
        self.record = record(
            self.config,
            covers=[cover(1), cover(2)],
            shots=[
                shot(0, block_id=1, step=1),
                shot(1, client_seq=1, arrival_seq=1, block_id=2, x=110, step=2),
                shot(2, client_seq=2, arrival_seq=2, block_id=1, step=3),
                shot(3, client_seq=3, arrival_seq=3, block_id=2, x=110, step=4),
                shot(4, client_seq=4, arrival_seq=4, block_id=1, step=5),
                shot(5, client_seq=5, arrival_seq=5, block_id=2, x=110, step=6),
            ],
        )
        self.direct = ReplayEngine(self.record, self.config).run()

    def test_resume_is_byte_identical_and_does_not_repeat_steps(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            interrupted = ReplayEngine(
                self.record, self.config, snapshot_dir=snapshots
            ).run(stop_after_step=3)
            self.assertFalse(interrupted.complete)
            self.assertEqual(interrupted.latest_snapshot_step, 3)
            resumed = ReplayEngine(
                self.record,
                self.config,
                snapshot_dir=snapshots,
                resume=True,
            ).run()
            self.assertTrue(resumed.complete)
            self.assertEqual(resumed.to_bytes(), self.direct.to_bytes())
            self.assertEqual(
                [frame["step"] for frame in resumed.frames], [1, 2, 3, 4, 5, 6]
            )

    def test_gap_reports_first_broken_step(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            engine = ReplayEngine(
                self.record, self.config, snapshot_dir=snapshots
            )
            engine.run(stop_after_step=4)
            (snapshots / "step-000002.json").unlink()
            store = SnapshotStore(
                snapshots,
                input_digest=engine.input_digest,
                config=self.config,
            )
            with self.assertRaises(RecoveryInvalidError) as context:
                store.load_latest()
            self.assertEqual(context.exception.details["first_broken_step"], 2)

    def test_payload_tamper_and_missing_field_are_recovery_invalid(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            engine = ReplayEngine(
                self.record, self.config, snapshot_dir=snapshots
            )
            engine.run(stop_after_step=4)
            path = snapshots / "step-000002.json"
            payload = load_json(path)
            payload["frame"]["state_hash"] = "f" * 64
            atomic_write_json(path, payload)
            with self.assertRaises(RecoveryInvalidError) as context:
                SnapshotStore(
                    snapshots,
                    input_digest=engine.input_digest,
                    config=self.config,
                ).load_latest()
            self.assertEqual(context.exception.details["first_broken_step"], 2)

            payload = load_json(path)
            del payload["ledger"]
            atomic_write_json(path, payload)
            with self.assertRaises(RecoveryInvalidError) as context:
                SnapshotStore(
                    snapshots,
                    input_digest=engine.input_digest,
                    config=self.config,
                ).load_latest()
            self.assertEqual(context.exception.details["first_broken_step"], 2)
            self.assertIn("ledger", context.exception.details["missing_fields"])

    def test_snapshot_version_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            engine = ReplayEngine(
                self.record, self.config, snapshot_dir=snapshots
            )
            engine.run(stop_after_step=2)
            version_two = replace(self.config, snapshot_version=2)
            with self.assertRaises(RecoveryInvalidError) as context:
                SnapshotStore(
                    snapshots,
                    input_digest=engine.input_digest,
                    config=version_two,
                ).load_latest()
            self.assertEqual(context.exception.details["first_broken_step"], 1)

    def test_input_digest_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            engine = ReplayEngine(
                self.record, self.config, snapshot_dir=snapshots
            )
            engine.run(stop_after_step=2)
            with self.assertRaises(RecoveryInvalidError) as context:
                SnapshotStore(
                    snapshots,
                    input_digest="different",
                    config=self.config,
                ).load_latest()
            self.assertEqual(context.exception.details["first_broken_step"], 1)

    def test_fresh_run_refuses_existing_snapshots(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            ReplayEngine(
                self.record, self.config, snapshot_dir=snapshots
            ).run(stop_after_step=1)
            with self.assertRaises(BusinessError) as context:
                ReplayEngine(self.record, self.config, snapshot_dir=snapshots)
            self.assertEqual(context.exception.code, "E_SNAPSHOT_EXISTS")

    def test_resume_requires_a_committed_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaises(RecoveryInvalidError) as context:
                ReplayEngine(
                    self.record,
                    self.config,
                    snapshot_dir=Path(temporary) / "empty",
                    resume=True,
                )
            self.assertEqual(context.exception.code, "E_RECOVERY_INVALID")
            self.assertEqual(context.exception.details["first_broken_step"], 1)

    def test_nested_state_missing_field_is_recovery_invalid(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            engine = ReplayEngine(
                self.record, self.config, snapshot_dir=snapshots
            )
            engine.run(stop_after_step=2)
            path = snapshots / "step-000002.json"
            payload = load_json(path)
            del payload["state"]["step"]
            atomic_write_json(path, payload)
            with self.assertRaises(RecoveryInvalidError) as context:
                SnapshotStore(
                    snapshots,
                    input_digest=engine.input_digest,
                    config=self.config,
                ).load_latest()
            self.assertEqual(context.exception.details["first_broken_step"], 2)

    def test_conflict_allow_run_resume_is_byte_identical(self) -> None:
        config_value = config()
        record_value = record(
            config_value,
            shots=[shot(0, predicted=51.0), shot(1, client_seq=1, arrival_seq=1)],
        )
        direct = ReplayEngine(
            record_value, config_value, allow_conflicts=True
        ).run()
        with tempfile.TemporaryDirectory() as temporary:
            snapshots = Path(temporary)
            interrupted = ReplayEngine(
                record_value,
                config_value,
                snapshot_dir=snapshots,
                allow_conflicts=True,
            ).run(stop_after_step=2)
            self.assertFalse(interrupted.complete)
            resumed = ReplayEngine(
                record_value,
                config_value,
                snapshot_dir=snapshots,
                resume=True,
                allow_conflicts=True,
            ).run()
        self.assertEqual(resumed.to_bytes(), direct.to_bytes())
        self.assertTrue(resumed.conflicts)


if __name__ == "__main__":
    unittest.main()
