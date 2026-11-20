"""要求 3：E_QUOTA_EXCEEDED 错误码与可归因字段。"""

import tempfile
import unittest

from replay_service.errors import E_QUOTA_EXCEEDED, ReplayError
from replay_service.runner import replay_exercise
from tests.test_engine_settlement import make_config, op


class QuotaExceededTest(unittest.TestCase):
    def test_strict_mode_raises_attributable_error(self):
        config = make_config("reject", quota=10.0, steps=2)
        operations = [
            op("o0", "A", 0, 1, 6.0),
            op("o1", "A", 1, 1, 7.0),
        ]
        with self.assertRaises(ReplayError) as ctx:
            replay_exercise(
                config,
                operations,
                checkpoint_dir=tempfile.mkdtemp(),
                strict_quota=True,
            )
        err = ctx.exception
        self.assertEqual(err.code, E_QUOTA_EXCEEDED)
        self.assertEqual(err.context["step"], 1)
        self.assertEqual(err.context["player_id"], "A")
        self.assertEqual(err.context["attempted_total"], 13.0)
        self.assertEqual(err.context["quota"], 10.0)
        self.assertEqual(err.context["rejected_op_ids"], ["o1"])
        self.assertTrue(err.context["op_client_seqs"])

    def test_default_mode_records_event_without_raising(self):
        config = make_config("degrade", quota=10.0, steps=1)
        outcome = replay_exercise(
            config, [op("o0", "A", 0, 0, 12.0)], strict_quota=False
        )
        self.assertEqual(outcome.results[0].quota_events[0].attempted_total, 12.0)


if __name__ == "__main__":
    unittest.main()
