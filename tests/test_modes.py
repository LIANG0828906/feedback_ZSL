from __future__ import annotations

import tempfile
import unittest
from pathlib import Path
from unittest import mock

from tickarena import (
    E_ARG,
    E_BUFFER,
    E_DIVERGE,
    E_MEMORY,
    E_OK,
    Engine,
    EngineLimits,
    InMemoryEventSource,
    compare_modes,
    generate_events,
    make_initial_state,
    verify_replay,
)
from tickarena.compare import ModeRun, run_mode


class ModeParityTests(unittest.TestCase):
    def test_tick_and_batch_are_identical_for_several_batch_sizes(self) -> None:
        initial = make_initial_state(
            seed=321,
            players=4,
            tick_count=700,
            win_score=1_000_000,
        )
        events = list(generate_events(initial, event_density="1.4"))
        for batch_size in (1, 7, 64, 257):
            with self.subTest(batch_size=batch_size):
                result = compare_modes(initial, events, batch_ticks=batch_size)
                self.assertEqual(result.code, E_OK, result)

    def test_comparator_returns_e_diverge_at_first_tick(self) -> None:
        initial = make_initial_state(
            seed=15,
            players=2,
            tick_count=20,
            win_score=100,
        )
        events = list(generate_events(initial, event_density="1.0"))
        left = run_mode(initial, events, "tick")
        right = run_mode(initial, events, "batch", batch_ticks=3)
        right.trace[4]["units"]["u1"]["hp"] += 11
        with mock.patch(
            "tickarena.compare.run_mode",
            side_effect=[left, right],
        ):
            result = compare_modes(initial, events, batch_ticks=3)
        self.assertEqual(result.code, E_DIVERGE)
        self.assertEqual(result.tick, 4)
        self.assertEqual(result.rule, "units.u1.hp")

    def test_event_buffer_overflow_is_bounded_and_verifiable(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial = make_initial_state(
                seed=17,
                players=2,
                tick_count=5,
                win_score=100,
            )
            events = [
                {"tick": 0, "type": "move", "actor": "u1", "dx": 1, "dy": 0},
                {"tick": 0, "type": "move", "actor": "u1", "dx": 1, "dy": 0},
                {"tick": 0, "type": "move", "actor": "u1", "dx": 1, "dy": 0},
            ]
            source = InMemoryEventSource(initial, events)
            replay = Path(tmp) / "bounded.replay.jsonl"
            limits = EngineLimits(event_buffer_limit=1, max_batch_ticks=2)
            engine = Engine(
                initial,
                source,
                replay_path=replay,
                snapshot_interval=2,
                limits=limits,
            )
            first = engine.run_batch(1)
            self.assertEqual(first.code, "E_MORE")
            self.assertEqual(first.rejection_counts.get(E_BUFFER), 2)
            while not engine.done:
                engine.run_batch(1)
            report = verify_replay(replay, source)
            self.assertEqual(report.code, E_OK, report)

    def test_batch_and_memory_limits_return_codes(self) -> None:
        initial = make_initial_state(
            seed=18,
            players=4,
            tick_count=10,
            win_score=100,
        )
        source = InMemoryEventSource(initial, [])
        engine = Engine(
            initial,
            source,
            limits=EngineLimits(
                max_batch_ticks=4,
                memory_limit_bytes=4_096,
            ),
        )
        too_large = engine.run_batch(5)
        self.assertEqual(too_large.code, E_ARG)
        low_memory = engine.run_batch(1)
        self.assertEqual(low_memory.code, E_MEMORY)


if __name__ == "__main__":
    unittest.main()

