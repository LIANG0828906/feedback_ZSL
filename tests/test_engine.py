from __future__ import annotations

import unittest

from sync_kernel.engine import (
    MatchEngine,
    advance_state,
    compute_input_fingerprint,
    deterministic_roll,
)
from sync_kernel.errors import E_SEMANTIC, KernelError
from sync_kernel.samples import build_sample_model


class EngineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        (
            cls.bundle,
            cls.context,
            cls.initial,
            cls.inputs,
            cls.states,
        ) = build_sample_model(ticks=30)

    def test_repeated_replay_has_identical_tick_hashes(self) -> None:
        first_engine = MatchEngine(self.context, self.initial, self.inputs)
        second_engine = MatchEngine(self.context, self.initial, self.inputs)
        first, _ = first_engine.run_to(30)
        second, _ = second_engine.run_to(30)
        self.assertEqual(
            [state.snapshot_hash for state in first],
            [state.snapshot_hash for state in second],
        )

    def test_random_source_depends_only_on_seed_tick_and_unit(self) -> None:
        before = deterministic_roll(self.context.seed, 11, "u2", "combat")
        engine = MatchEngine(self.context, self.initial, self.inputs)
        engine.run_to(17)
        after = deterministic_roll(self.context.seed, 11, "u2", "combat")
        self.assertEqual(before, after)
        self.assertNotEqual(
            deterministic_roll(self.context.seed, 11, "u2", "combat"),
            deterministic_roll(self.context.seed, 12, "u2", "combat"),
        )

    def test_state_uses_integer_fixed_point_fields(self) -> None:
        for state in self.states:
            for unit in state.units.values():
                for field_name in (
                    "x",
                    "y",
                    "heading",
                    "hp",
                    "max_hp",
                    "cooldown",
                    "resource",
                    "status_mask",
                ):
                    self.assertIsInstance(getattr(unit, field_name), int)

    def test_unknown_input_unit_is_semantic_error(self) -> None:
        with self.assertRaises(KernelError) as raised:
            advance_state(
                self.initial,
                self.context,
                [{"unit_id": "ghost", "dx": 1, "dy": 0}],
            )
        self.assertEqual(raised.exception.code, E_SEMANTIC)
        self.assertEqual(raised.exception.issue.object_id, "ghost")

    def test_input_fingerprint_is_stable(self) -> None:
        first = compute_input_fingerprint(self.context, self.initial, self.inputs)
        second = compute_input_fingerprint(self.context, self.initial, self.inputs)
        self.assertEqual(first, second)
        self.assertEqual(first, self.context.input_fingerprint)


if __name__ == "__main__":
    unittest.main()

