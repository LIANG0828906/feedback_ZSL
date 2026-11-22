from __future__ import annotations

import tempfile
import time
import unittest
from pathlib import Path

import numpy as np

from tickarena import (
    E_OK,
    Engine,
    JsonlEventSource,
    generate_events,
    make_initial_state,
    verify_replay,
)
from tickarena.generator import write_generated_match
from tickarena.replay import iter_replay_body, load_and_validate_replay


class GeneratorAndScaleTests(unittest.TestCase):
    def test_generator_is_reproducible(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial = make_initial_state(
                seed=808,
                players=4,
                tick_count=2_000,
                win_score=1_000_000,
            )
            first = Path(tmp) / "first.jsonl"
            second = Path(tmp) / "second.jsonl"
            third = Path(tmp) / "third.jsonl"
            hash_one = write_generated_match(first, initial, event_density="1.25")
            hash_two = write_generated_match(second, initial, event_density="1.25")
            other = make_initial_state(
                seed=809,
                players=4,
                tick_count=2_000,
                win_score=1_000_000,
            )
            hash_other = write_generated_match(third, other, event_density="1.25")
            self.assertEqual(hash_one, hash_two)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            self.assertNotEqual(hash_one, hash_other)

    def test_numpy_integer_curves_match_for_tick_and_batch(self) -> None:
        from tickarena import compare_modes

        initial = make_initial_state(
            seed=811,
            players=4,
            tick_count=600,
            win_score=1_000_000,
        )
        events = list(generate_events(initial, event_density="1.3"))
        comparison = compare_modes(initial, events, batch_ticks=31)
        self.assertEqual(comparison.code, E_OK)

        from tickarena.compare import run_mode

        tick_trace = run_mode(initial, events, "tick").trace
        batch_trace = run_mode(initial, events, "batch", batch_ticks=31).trace
        tick_hp = np.asarray(
            [[row["units"][unit]["hp"] for unit in ("u1", "u2", "u3", "u4")] for row in tick_trace],
            dtype=np.int64,
        )
        batch_hp = np.asarray(
            [[row["units"][unit]["hp"] for unit in ("u1", "u2", "u3", "u4")] for row in batch_trace],
            dtype=np.int64,
        )
        self.assertTrue(np.array_equal(tick_hp, batch_hp))
        self.assertEqual(tick_hp.dtype, np.int64)

    def test_ten_thousand_tick_batch_recording_and_verification(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial = make_initial_state(
                seed=812,
                players=4,
                tick_count=10_000,
                win_score=1_000_000,
            )
            event_path = Path(tmp) / "large.events.jsonl"
            write_generated_match(event_path, initial, event_density="0.8")
            replay = Path(tmp) / "large.replay.jsonl"
            source = JsonlEventSource(event_path)
            engine = Engine(
                initial,
                source,
                replay_path=replay,
                snapshot_interval=500,
            )
            started = time.perf_counter()
            while not engine.done:
                result = engine.run_batch(1_024)
                self.assertIn(result.code, {E_OK, "E_MORE"})
            elapsed = time.perf_counter() - started
            self.assertEqual(engine.state.tick, 9_999)
            meta = load_and_validate_replay(replay)
            self.assertEqual(meta.footer["final_tick"], 9_999)
            tick_records = sum(
                1 for record in iter_replay_body(replay) if record.get("kind") == "tick"
            )
            self.assertEqual(tick_records, 10_000)
            report = verify_replay(replay, event_path)
            self.assertEqual(report.code, E_OK, report)
            self.assertGreaterEqual(elapsed, 0.0)
            engine.close()


if __name__ == "__main__":
    unittest.main()

