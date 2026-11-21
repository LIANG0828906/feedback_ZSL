from __future__ import annotations

import unittest

from cover_replay.engine import ReplayEngine, Withdrawal
from cover_replay.errors import (
    BusyError,
    CrackUnstableError,
    CreditConflictError,
    MemoryBudgetError,
)

from tests.common import config, cover, record, shot


class EngineTests(unittest.TestCase):
    def test_repeated_replay_is_byte_identical(self) -> None:
        config_value = config()
        record_value = record(
            config_value,
            shots=[
                shot(0, player_id="P02", client_seq=0, arrival_seq=1),
                shot(1, player_id="P01", client_seq=0, arrival_seq=0, x=20),
            ],
        )
        first = ReplayEngine(record_value, config_value).run().to_bytes()
        second = ReplayEngine(record_value, config_value).run().to_bytes()
        self.assertEqual(first, second)

    def test_stable_player_client_sort_and_spread_damage(self) -> None:
        config_value = config()
        record_value = record(
            config_value,
            covers=[cover(1), cover(2)],
            shots=[
                shot(
                    0,
                    player_id="P02",
                    client_seq=4,
                    arrival_seq=0,
                    block_id=1,
                    momentum=60,
                ),
                shot(
                    1,
                    player_id="P01",
                    client_seq=9,
                    arrival_seq=1,
                    block_id=1,
                    momentum=40,
                ),
            ],
        )
        frame = ReplayEngine(record_value, config_value).advance()
        self.assertEqual(
            [(item["player_id"], item["client_sequence"]) for item in frame["effective_shots"]],
            [("P01", 9), ("P02", 4)],
        )
        self.assertTrue(frame["secondary_damage"])
        self.assertEqual(frame["secondary_damage"][0]["block_id"], 2)

    def test_budget_waves_queue_watermark_and_commit_order(self) -> None:
        config_value = config(memory_budget_per_step=2, max_workers=2)
        covers = [cover(index, memory_units=2) for index in range(1, 4)]
        record_value = record(config_value, covers=covers, shots=[shot(0, block_id=1)])
        frame = ReplayEngine(record_value, config_value).advance()
        usage = frame["budget_usage"]
        self.assertEqual(usage["peak_memory_used"], 2)
        self.assertEqual(usage["queue_watermark"], 2)
        commits = [
            event
            for event in frame["scheduling"]["trace"]
            if event["event"] == "commit"
        ]
        committed = [block for event in commits for block in event["commit_order"]]
        self.assertEqual(committed, [1, 2, 3])

    def test_task_over_budget_does_not_raise_limits(self) -> None:
        config_value = config(memory_budget_per_step=1, max_workers=4)
        record_value = record(
            config_value,
            covers=[cover(1, memory_units=2)],
            shots=[shot(0)],
        )
        with self.assertRaises(MemoryBudgetError) as context:
            ReplayEngine(record_value, config_value).advance()
        self.assertEqual(context.exception.code, "E_MEMORY_BUDGET")

    def test_duplicate_step_returns_busy(self) -> None:
        config_value = config()
        record_value = record(config_value)
        engine = ReplayEngine(record_value, config_value)
        engine.advance()
        with self.assertRaises(BusyError) as context:
            engine.advance(expected_step=1)
        self.assertEqual(context.exception.code, "E_BUSY")

    def test_credit_conflict_payload_and_integrity(self) -> None:
        config_value = config()
        record_value = record(
            config_value,
            shots=[shot(0, predicted=51.0)],
        )
        with self.assertRaises(CreditConflictError) as context:
            ReplayEngine(record_value, config_value).advance()
        error = context.exception
        self.assertEqual(error.code, "E_CREDIT_CONFLICT")
        self.assertEqual(error.details["block_id"], 1)
        self.assertEqual(error.details["shot_index"], 0)
        self.assertIn("predicted_remaining", error.details)
        self.assertIn("authoritative_remaining", error.details)

        run = ReplayEngine(
            record_value, config_value, allow_conflicts=True
        ).run()
        self.assertTrue(run.complete)
        self.assertTrue(run.conflicts)
        durability = {item.block_id: item.durability_max for item in record_value.covers}
        for frame in run.frames:
            for state in frame["covers"]:
                maximum = durability[state["block_id"]]
                self.assertEqual(
                    round(
                        float(state["remaining"])
                        + float(state["accumulated_damage"]),
                        3,
                    ),
                    round(maximum / 1000, 3),
                )
                self.assertGreaterEqual(float(state["remaining"]), 0.0)

    def test_zero_authority_hit_is_refunded_not_conflict(self) -> None:
        config_value = config()
        record_value = record(
            config_value,
            shots=[shot(0, momentum=0, predicted=20)],
        )
        run = ReplayEngine(record_value, config_value).run()
        self.assertFalse(run.conflicts)
        self.assertEqual(
            run.frames[0]["credit_entries"][0]["status"], "refunded"
        )
        self.assertEqual(
            run.frames[0]["covers"][0]["accumulated_damage"], "0.000"
        )

    def test_withdrawal_compensation_increases_remaining(self) -> None:
        config_value = config()
        record_value = record(
            config_value,
            shots=[shot(0), shot(1, client_seq=1, arrival_seq=1, step=1)],
        )
        baseline = ReplayEngine(record_value, config_value).run()
        compensated = ReplayEngine(
            record_value,
            config_value,
            withdrawals=(Withdrawal(step=2, block_id=1, amount="10.000"),),
        ).run()
        base_remaining = float(baseline.frames[-1]["covers"][0]["remaining"])
        compensated_remaining = float(
            compensated.frames[-1]["covers"][0]["remaining"]
        )
        self.assertAlmostEqual(
            compensated_remaining - base_remaining, 10.0, places=3
        )
        self.assertEqual(
            compensated.frames[1]["withdrawals"][0]["refunded"], "10.000"
        )

    def test_crack_propagation_unstable(self) -> None:
        covers = [
            cover(block_id, x=block_id * 50, width=40, height=40, durability=100,
                  threshold=20, spread_radius=10)
            for block_id in range(1, 5)
        ]
        config_value = config(
            spread_damage_percent=0,
            crack_propagation_damage="100",
            max_crack_iterations=1,
        )
        record_value = record(
            config_value,
            covers=covers,
            shots=[shot(0, x=60, y=10)],
        )
        with self.assertRaises(CrackUnstableError) as context:
            ReplayEngine(record_value, config_value).advance()
        error = context.exception
        self.assertEqual(error.code, "E_CRACK_UNSTABLE")
        self.assertTrue(error.details["block_ids"])
        self.assertTrue(error.details["propagation_chain"])

    def test_crack_propagation_stable(self) -> None:
        covers = [
            cover(block_id, x=block_id * 50, width=40, height=40, durability=100,
                  threshold=20, spread_radius=10)
            for block_id in range(1, 5)
        ]
        config_value = config(
            spread_damage_percent=0,
            crack_propagation_damage="30",
            max_crack_iterations=8,
        )
        record_value = record(
            config_value,
            covers=covers,
            shots=[shot(0, x=60, y=10)],
        )
        frame = ReplayEngine(record_value, config_value).advance()
        causes = {event["cause"] for event in frame["crack_events"]}
        self.assertIn("threshold", causes)
        self.assertIn("crack_shock", causes)


if __name__ == "__main__":
    unittest.main()
