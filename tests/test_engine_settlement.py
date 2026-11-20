"""要求 2/3：结算引擎、稳定排序、透支归因与在线/离线一致性。"""

import unittest

from replay_service.engine import QuotaReplayEngine
from replay_service.models import (
    POLICY_DEGRADE,
    POLICY_REJECT,
    ExerciseConfig,
    Operation,
    PenaltyRule,
)


def make_config(policy="reject", quota=10.0, steps=3, players=("A", "B")):
    return ExerciseConfig(
        exercise_id="ex",
        total_steps=steps,
        per_step_quota=quota,
        player_ids=tuple(players),
        penalty_rule=PenaltyRule(
            policy=policy, reject_penalty=2.0, degrade_rate=1.5, degrade_fixed=1.0
        ),
    )


def op(op_id, player, seq, step, compute):
    return Operation(
        op_id=op_id,
        player_id=player,
        client_seq=seq,
        effective_step=step,
        required_compute=compute,
        client_ts=1000.0,
        arrival_ts=2000.0,
    )


class EngineSettlementTest(unittest.TestCase):
    def test_reject_policy_attributes_by_client_seq(self):
        config = make_config(POLICY_REJECT)
        operations = [
            op("o2", "A", 2, 0, 6.0),
            op("o1", "A", 1, 0, 5.0),
            op("o0", "A", 0, 0, 4.0),
        ]
        engine = QuotaReplayEngine(config, operations)
        result = engine.step()
        # 按 client_seq 排序：o0(4) 接受，o1(4+5=9) 接受，o2(9+6=15) 拒绝
        self.assertEqual(result.step_used["A"], 9.0)
        self.assertEqual(len(result.penalties), 1)
        self.assertEqual(result.penalties[0].op_id, "o2")
        self.assertEqual(result.penalties[0].deduction, 2.0)
        event = result.quota_events[0]
        self.assertEqual(event.attempted_total, 15.0)
        self.assertEqual(event.quota, 10.0)
        self.assertEqual(event.player_id, "A")
        self.assertEqual(event.rejected_op_ids, ["o2"])

    def test_arrival_order_does_not_matter(self):
        config = make_config(POLICY_REJECT)
        ascending = [op(f"o{i}", "A", i, 0, 4.0) for i in range(3)]
        descending = list(reversed(ascending))
        r1 = QuotaReplayEngine(config, ascending).run()
        r2 = QuotaReplayEngine(config, descending).run()
        self.assertEqual(
            [r.step_hash for r in r1], [r.step_hash for r in r2]
        )
        self.assertEqual(r1[0].step_used, r2[0].step_used)

    def test_degrade_policy_allows_all_and_charges_overage(self):
        config = make_config(POLICY_DEGRADE, quota=10.0)
        operations = [op("o0", "A", 0, 0, 7.0), op("o1", "A", 1, 0, 8.0)]
        result = QuotaReplayEngine(config, operations).step()
        self.assertEqual(result.step_used["A"], 15.0)
        self.assertEqual(len(result.penalties), 1)
        # fixed 1.0 + rate 1.5 * overage 5.0 = 8.5
        self.assertEqual(result.penalties[0].deduction, 8.5)
        self.assertEqual(result.quota_events[0].policy, POLICY_DEGRADE)

    def test_online_step_matches_offline_run(self):
        config = make_config(POLICY_REJECT, steps=5)
        operations = [
            op(f"o{i}", "AB"[i % 2], i // 2, i % 5, float(3 + i % 4))
            for i in range(12)
        ]
        offline = QuotaReplayEngine(config, operations).run()

        online_engine = QuotaReplayEngine(config, operations)
        online = []
        for _ in range(config.total_steps):
            online.append(online_engine.step())
        self.assertEqual(
            [r.step_hash for r in offline], [r.step_hash for r in online]
        )
        self.assertEqual(
            [r.cumulative_used for r in offline],
            [r.cumulative_used for r in online],
        )

    def test_timestamps_never_enter_hash(self):
        config = make_config(POLICY_REJECT)
        operations = [op("o0", "A", 0, 0, 4.0)]
        r1 = QuotaReplayEngine(config, operations).run()
        operations[0] = Operation(
            op_id="o0",
            player_id="A",
            client_seq=0,
            effective_step=0,
            required_compute=4.0,
            client_ts=999999.0,
            arrival_ts=0.0001,
        )
        r2 = QuotaReplayEngine(config, operations).run()
        self.assertEqual(r1[0].step_hash, r2[0].step_hash)

    def test_empty_step_still_produces_hash_and_zero_usage(self):
        config = make_config(POLICY_REJECT, steps=2)
        result = QuotaReplayEngine(config, []).run()
        self.assertEqual(len(result), 2)
        self.assertEqual(result[0].step_used, {"A": 0.0, "B": 0.0})
        self.assertEqual(len(result[0].step_hash), 64)
        self.assertNotEqual(result[0].step_hash, result[1].step_hash)


if __name__ == "__main__":
    unittest.main()
