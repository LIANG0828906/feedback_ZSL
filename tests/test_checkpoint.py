from __future__ import annotations

import tempfile
import threading
import unittest
from pathlib import Path

from sync_kernel.checkpoint import CheckpointStore, resume_match
from sync_kernel.engine import MatchEngine
from sync_kernel.errors import E_SEQ
from sync_kernel.models import MatchContext
from sync_kernel.samples import build_sample_model


class CheckpointTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        (
            cls.bundle,
            cls.context,
            cls.initial,
            cls.inputs,
            cls.states,
        ) = build_sample_model(ticks=30)
        cls.engine = MatchEngine(cls.context, cls.initial, cls.inputs)

    def test_atomic_checkpoint_and_serial_commit_order(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            store = CheckpointStore(
                Path(temporary) / "checkpoint.json",
                context=self.context,
                protocol_version=3,
                commit_window=10,
            )
            store.save(self.states[0])
            store.save(self.states[10])
            store.save(self.states[20])
            loaded = store.load()
            self.assertTrue(loaded.valid)
            self.assertEqual(loaded.state.tick, 20)
            with self.assertRaises(Exception) as raised:
                store.save(self.states[10])
            self.assertEqual(getattr(raised.exception, "code", ""), E_SEQ)

    def test_identity_mismatch_invalidates_whole_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "checkpoint.json"
            good = CheckpointStore(
                path,
                context=self.context,
                protocol_version=3,
                commit_window=10,
            )
            good.save(self.states[20])
            bad_context = MatchContext(
                match_id=self.context.match_id,
                seed=self.context.seed + 1,
                tick_rate=self.context.tick_rate,
                clock_hz=self.context.clock_hz,
                input_fingerprint=self.context.input_fingerprint,
            )
            bad = CheckpointStore(
                path,
                context=bad_context,
                protocol_version=3,
                commit_window=10,
            )
            loaded = bad.load()
            self.assertFalse(loaded.valid)
            self.assertTrue(loaded.recompute_required)
            fingerprint_context = MatchContext(
                match_id=self.context.match_id,
                seed=self.context.seed,
                tick_rate=self.context.tick_rate,
                clock_hz=self.context.clock_hz,
                input_fingerprint="0" * 64,
            )
            fingerprint_store = CheckpointStore(
                path,
                context=fingerprint_context,
                protocol_version=3,
                commit_window=10,
            )
            fingerprint_load = fingerprint_store.load()
            self.assertFalse(fingerprint_load.valid)
            self.assertIn(
                "input_fingerprint",
                fingerprint_load.details["mismatches"],
            )
            protocol_store = CheckpointStore(
                path,
                context=self.context,
                protocol_version=1,
                commit_window=10,
            )
            protocol_load = protocol_store.load()
            self.assertFalse(protocol_load.valid)
            self.assertIn(
                "protocol_version",
                protocol_load.details["mismatches"],
            )
            recomputed = resume_match(
                engine=self.engine,
                store=bad,
                target_tick=20,
                save_commits=False,
            )
            self.assertEqual(
                recomputed.final_state.snapshot_hash,
                self.states[20].snapshot_hash,
            )

    def test_forced_termination_uses_only_last_persisted_window(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            store = CheckpointStore(
                Path(temporary) / "forced.json",
                context=self.context,
                protocol_version=3,
                commit_window=10,
            )
            terminated = resume_match(
                engine=self.engine,
                store=store,
                target_tick=30,
                force_terminate_at=27,
            )
            self.assertFalse(terminated.complete)
            persisted = store.load()
            self.assertTrue(persisted.valid)
            self.assertEqual(persisted.state.tick, 20)
            restarted = resume_match(
                engine=self.engine,
                store=store,
                target_tick=30,
            )
            self.assertTrue(restarted.complete)
            self.assertEqual(restarted.resumed_tick, 20)
            self.assertEqual(
                restarted.final_state.snapshot_hash,
                self.states[30].snapshot_hash,
            )

    def test_cancellation_does_not_claim_completion(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            store = CheckpointStore(
                Path(temporary) / "cancel.json",
                context=self.context,
                protocol_version=3,
                commit_window=10,
            )
            store.save(self.states[20])
            cancelled = threading.Event()
            cancelled.set()
            result = resume_match(
                engine=self.engine,
                store=store,
                target_tick=30,
                cancel_event=cancelled,
            )
            self.assertTrue(result.cancelled)
            self.assertFalse(result.complete)
            self.assertEqual(result.final_state.tick, 20)

    def test_corrupt_checkpoint_is_rejected_as_a_whole(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "corrupt.json"
            store = CheckpointStore(
                path,
                context=self.context,
                protocol_version=3,
                commit_window=10,
            )
            store.save(self.states[10])
            path.write_text('{"format":', encoding="utf-8")
            loaded = store.load()
            self.assertFalse(loaded.valid)
            self.assertEqual(loaded.reason, "E_DECODE")


if __name__ == "__main__":
    unittest.main()

