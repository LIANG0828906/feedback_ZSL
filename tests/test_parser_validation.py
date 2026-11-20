"""要求 1：配置与操作记录解析、严格校验、首个出错位置定位。"""

import unittest

from replay_service.errors import E_INPUT_INVALID, ReplayError
from replay_service.parser import parse_input


def base_payload(**overrides):
    payload = {
        "config": {
            "exercise_id": "ex-1",
            "total_steps": 4,
            "per_step_quota": 10.0,
            "player_ids": ["A", "B"],
            "penalty_rule": {
                "policy": "reject",
                "reject_penalty": 2.0,
                "degrade_rate": 1.5,
                "degrade_fixed": 1.0,
            },
        },
        "operations": [
            {
                "op_id": "op0",
                "player_id": "A",
                "client_seq": 0,
                "effective_step": 0,
                "required_compute": 3.0,
            }
        ],
    }
    payload.update(overrides)
    return payload


class ParserValidationTest(unittest.TestCase):
    def test_valid_input_parses(self):
        config, operations = parse_input(base_payload())
        self.assertEqual(config.total_steps, 4)
        self.assertEqual(operations[0].required_compute, 3.0)

    def test_missing_config_field_reports_location(self):
        payload = base_payload()
        del payload["config"]["total_steps"]
        with self.assertRaises(ReplayError) as ctx:
            parse_input(payload)
        self.assertEqual(ctx.exception.code, E_INPUT_INVALID)
        self.assertIn("config.total_steps", ctx.exception.context["location"])

    def test_step_out_of_range_points_to_record(self):
        payload = base_payload()
        payload["operations"][0]["effective_step"] = 9
        with self.assertRaises(ReplayError) as ctx:
            parse_input(payload)
        self.assertEqual(ctx.exception.code, E_INPUT_INVALID)
        self.assertEqual(ctx.exception.context["location"], "operations[0].effective_step")

    def test_negative_compute_rejected(self):
        payload = base_payload()
        payload["operations"][0]["required_compute"] = -1.0
        with self.assertRaises(ReplayError) as ctx:
            parse_input(payload)
        self.assertEqual(ctx.exception.code, E_INPUT_INVALID)
        self.assertIn("operations[0].required_compute", ctx.exception.context["location"])

    def test_unknown_player_rejected(self):
        payload = base_payload()
        payload["operations"][0]["player_id"] = "Z"
        with self.assertRaises(ReplayError) as ctx:
            parse_input(payload)
        self.assertIn("operations[0].player_id", ctx.exception.context["location"])

    def test_duplicate_client_seq_rejected(self):
        payload = base_payload()
        payload["operations"].append(
            {
                "op_id": "op1",
                "player_id": "A",
                "client_seq": 0,
                "effective_step": 1,
                "required_compute": 1.0,
            }
        )
        with self.assertRaises(ReplayError) as ctx:
            parse_input(payload)
        self.assertEqual(ctx.exception.context["location"], "operations[1]")

    def test_first_error_location_is_reported(self):
        payload = base_payload()
        payload["operations"][0]["client_seq"] = -2
        payload["operations"].append(
            {
                "op_id": "bad",
                "player_id": "B",
                "client_seq": 0,
                "effective_step": 100,
                "required_compute": -5.0,
            }
        )
        with self.assertRaises(ReplayError) as ctx:
            parse_input(payload)
        self.assertEqual(ctx.exception.context["location"], "operations[0].client_seq")

    def test_boolean_rejected_as_number(self):
        payload = base_payload()
        payload["config"]["per_step_quota"] = True
        with self.assertRaises(ReplayError):
            parse_input(payload)

    def test_unknown_policy_rejected(self):
        payload = base_payload()
        payload["config"]["penalty_rule"]["policy"] = "rollback"
        with self.assertRaises(ReplayError) as ctx:
            parse_input(payload)
        self.assertIn("config.penalty_rule.policy", ctx.exception.context["location"])


if __name__ == "__main__":
    unittest.main()
