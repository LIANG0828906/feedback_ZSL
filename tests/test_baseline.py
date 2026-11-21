from __future__ import annotations

import copy
import unittest

from cover_replay.baseline import (
    build_baseline,
    compare_baseline,
    compare_strategies,
)
from cover_replay.engine import ReplayEngine
from cover_replay.errors import BaselineDivergenceError, BaselineVersionError

from tests.common import config, cover, record, shot


class BaselineTests(unittest.TestCase):
    def setUp(self) -> None:
        self.config = config()
        self.record = record(
            self.config,
            covers=[cover(1), cover(2)],
            shots=[
                shot(0),
                shot(1, client_seq=1, arrival_seq=1, momentum=40),
            ],
        )
        self.run = ReplayEngine(self.record, self.config).run()
        self.baseline = build_baseline(self.run)

    def test_matching_baseline(self) -> None:
        result = compare_baseline(self.run, self.baseline)
        self.assertTrue(result["ok"])
        self.assertEqual(result["steps_compared"], 4)

    def test_version_mismatch(self) -> None:
        altered = copy.deepcopy(self.baseline)
        altered["model_version"] = "cover-damage-v2"
        with self.assertRaises(BaselineVersionError) as context:
            compare_baseline(self.run, altered)
        self.assertEqual(context.exception.code, "E_BASELINE_VERSION")

    def test_first_divergence_diagnostic(self) -> None:
        altered = copy.deepcopy(self.baseline)
        altered["frames"][1]["state_hash"] = "0" * 64
        with self.assertRaises(BaselineDivergenceError) as context:
            compare_baseline(self.run, altered)
        error = context.exception
        self.assertEqual(error.code, "E_BASELINE_DIVERGENCE")
        self.assertEqual(error.details["first_mismatch_step"], 2)
        self.assertIn("state_hash", error.details["field_path"])
        self.assertIsInstance(error.details["effective_client_sequences"], list)
        self.assertIn("queue_watermark", error.details)
        self.assertEqual(error.details["attribution"], "fixed_point_damage_or_reduction_order")

    def test_strategy_equivalence_and_adaptive_choice(self) -> None:
        shots = [
            shot(
                index,
                client_seq=index,
                arrival_seq=index,
                block_id=1 + (index % 2),
                x=10 + ((index % 2) * 100),
                momentum=20 + (index % 7),
            )
            for index in range(20)
        ]
        record_value = record(
            self.config,
            covers=[cover(1), cover(2)],
            shots=shots,
        )
        result = compare_strategies(record_value, self.config)
        self.assertTrue(result["ok"])
        full = ReplayEngine(
            record_value, self.config.with_strategy("full")
        ).run()
        window = ReplayEngine(
            record_value, self.config.with_strategy("window")
        ).run()
        self.assertEqual(full.frames, window.frames)
        adaptive_full = ReplayEngine(
            record_value,
            self.config.__class__(**{**self.config.to_dict(), "full_scan_cost_limit": 10000}),
        ).run()
        adaptive_window = ReplayEngine(
            record_value,
            self.config.__class__(**{**self.config.to_dict(), "full_scan_cost_limit": 1}),
        ).run()
        self.assertEqual(adaptive_full.strategy, "full")
        self.assertEqual(adaptive_window.strategy, "window")


if __name__ == "__main__":
    unittest.main()
