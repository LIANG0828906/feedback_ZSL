from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from tickarena import (
    E_CANCELLED,
    E_OK,
    E_ORDER,
    E_PAUSED,
    CheckpointStore,
    Engine,
    EngineLimits,
    JsonlEventSource,
    verify_replay,
)
from tests.support import write_match


class CheckpointTests(unittest.TestCase):
    def test_pause_checkpoint_resume_matches_uninterrupted_run(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial, event_path = write_match(
                tmp,
                seed=501,
                players=4,
                ticks=700,
                density="1.1",
            )
            baseline_source = JsonlEventSource(event_path)
            baseline_replay = Path(tmp) / "baseline.replay.jsonl"
            baseline = Engine(
                initial,
                baseline_source,
                replay_path=baseline_replay,
                snapshot_interval=40,
            )
            while not baseline.done:
                batch = baseline.run_batch(73)
                self.assertIn(batch.code, {E_OK, "E_MORE"})
            baseline_state = baseline.full_snapshot()
            baseline.close()

            store = CheckpointStore(Path(tmp) / "store")
            paused_replay = Path(tmp) / "resumed.replay.jsonl"
            source = JsonlEventSource(event_path)
            engine = Engine(
                initial,
                source,
                replay_path=paused_replay,
                snapshot_interval=40,
                checkpoint_store=store,
                checkpoint_every=64,
            )
            for _ in range(4):
                engine.run_batch(64)
            engine.request_pause()
            paused = engine.run_batch(64)
            self.assertEqual(paused.code, E_PAUSED)
            self.assertEqual(store.last_tick, paused.tick)
            engine.close()

            resumed_source = JsonlEventSource(event_path)
            resumed, info = Engine.resume(
                initial,
                resumed_source,
                checkpoint_store=store,
                replay_path=paused_replay,
                snapshot_interval=40,
                checkpoint_every=64,
            )
            self.assertTrue(info.resumed)
            self.assertFalse(info.restarted)
            self.assertEqual(info.code, E_OK)
            while not resumed.done:
                batch = resumed.run_batch(128)
                self.assertIn(batch.code, {E_OK, "E_MORE"})
            self.assertEqual(resumed.full_snapshot(), baseline_state)
            self.assertEqual(verify_replay(paused_replay, event_path).code, E_OK)
            self.assertEqual(verify_replay(baseline_replay, event_path).code, E_OK)
            resumed.close()

    def test_event_fingerprint_mismatch_invalidates_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial, event_path = write_match(
                tmp,
                seed=601,
                players=2,
                ticks=200,
                density="0.8",
            )
            store = CheckpointStore(Path(tmp) / "store")
            replay = Path(tmp) / "match.replay.jsonl"
            source = JsonlEventSource(event_path)
            engine = Engine(
                initial,
                source,
                replay_path=replay,
                snapshot_interval=20,
                checkpoint_store=store,
                checkpoint_every=32,
            )
            engine.run_batch(64)
            engine.request_pause()
            self.assertEqual(engine.run_batch(64).code, E_PAUSED)
            engine.close()

            changed = Path(tmp) / "changed.events.jsonl"
            changed_events = [
                {"tick": tick, "type": "move", "actor": "u1", "dx": 1, "dy": 0}
                for tick in range(0, 200, 2)
            ]
            from tickarena.events import write_event_stream

            write_event_stream(changed, initial, iter(changed_events))
            changed_source = JsonlEventSource(changed)
            restarted, info = Engine.resume(
                initial,
                changed_source,
                checkpoint_store=store,
                replay_path=replay,
                snapshot_interval=20,
                checkpoint_every=32,
            )
            self.assertFalse(info.resumed)
            self.assertTrue(info.restarted)
            self.assertEqual(info.code, "E_STATE")
            self.assertEqual(restarted.state.tick, -1)
            self.assertTrue((Path(tmp) / "store" / "checkpoint.invalid.json").is_file())
            restarted.close()

    def test_checkpoint_commits_are_atomic_and_monotonic(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            store = CheckpointStore(tmp)
            self.assertEqual(store.commit({"value": "one"}, 1).code, E_OK)
            self.assertEqual(store.commit({"value": "three"}, 3).code, E_OK)
            stale = store.commit({"value": "two"}, 2)
            self.assertEqual(stale.code, E_ORDER)
            loaded = store.load()
            self.assertTrue(loaded.result.ok)
            self.assertEqual(loaded.envelope["engine_tick"], 3)
            self.assertEqual(loaded.payload["value"], "three")
            self.assertFalse(any(path.name.endswith(".tmp") for path in Path(tmp).iterdir()))

    def test_fresh_engine_discards_previous_run_checkpoint(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial, _event_path = write_match(
                tmp,
                seed=625,
                players=2,
                ticks=20,
                density="0",
            )
            store = CheckpointStore(Path(tmp) / "store")
            self.assertEqual(store.commit({"old": True}, 19).code, E_OK)
            source = JsonlEventSource(_event_path)
            engine = Engine(initial, source, checkpoint_store=store)
            self.assertEqual(store.last_tick, -1)
            self.assertTrue((Path(tmp) / "store" / "checkpoint.invalid.json").is_file())
            engine.close()

    def test_replay_version_mismatch_forces_full_replay(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial, event_path = write_match(
                tmp,
                seed=651,
                players=2,
                ticks=160,
                density="0.9",
            )
            store = CheckpointStore(Path(tmp) / "store")
            replay = Path(tmp) / "match.replay.jsonl"
            source = JsonlEventSource(event_path)
            engine = Engine(
                initial,
                source,
                replay_path=replay,
                snapshot_interval=20,
                checkpoint_store=store,
                checkpoint_every=32,
            )
            engine.run_batch(40)
            engine.request_pause()
            self.assertEqual(engine.run_batch(32).code, E_PAUSED)
            engine.close()
            checkpoint_path = Path(tmp) / "store" / "checkpoint.json"
            envelope = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            envelope["payload"]["replay_format_version"] = 999
            checkpoint_path.write_text(
                json.dumps(envelope, sort_keys=True, separators=(",", ":")) + "\n",
                encoding="utf-8",
            )
            resumed_source = JsonlEventSource(event_path)
            restarted, info = Engine.resume(
                initial,
                resumed_source,
                checkpoint_store=store,
                replay_path=replay,
                snapshot_interval=20,
                checkpoint_every=32,
            )
            self.assertTrue(info.restarted)
            self.assertEqual(info.code, "E_STATE")
            self.assertEqual(restarted.state.tick, -1)
            restarted.close()

    def test_cancel_returns_code_and_persists_intermediate_state(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial, event_path = write_match(
                tmp,
                seed=701,
                players=2,
                ticks=300,
                density="1.0",
            )
            store = CheckpointStore(Path(tmp) / "store")
            source = JsonlEventSource(event_path)
            engine = Engine(
                initial,
                source,
                replay_path=Path(tmp) / "cancel.replay.jsonl",
                checkpoint_store=store,
                checkpoint_every=64,
            )
            engine.run_batch(20)
            engine.request_cancel()
            cancelled = engine.run_batch(64)
            self.assertEqual(cancelled.code, E_CANCELLED)
            self.assertGreater(cancelled.tick, 0)
            self.assertEqual(store.last_tick, cancelled.tick)
            engine.close()


if __name__ == "__main__":
    unittest.main()

