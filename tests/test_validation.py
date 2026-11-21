from __future__ import annotations

import unittest

from cover_replay.config import parse_config
from cover_replay.errors import ConfigInvalid, RecordInvalid
from cover_replay.model import parse_record

from tests.common import config_payload, cover, record_payload, shot


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.config = parse_config(config_payload())

    def assert_invalid(
        self,
        payload: dict,
        *,
        shot_index: int | None = None,
        field_part: str,
    ) -> RecordInvalid:
        with self.assertRaises(RecordInvalid) as context:
            parse_record(payload, self.config)
        error = context.exception
        self.assertEqual(error.code, "E_RECORD_INVALID")
        self.assertIn(field_part, error.details["field"])
        if shot_index is not None:
            self.assertEqual(error.details["shot_index"], shot_index)
        return error

    def test_missing_shot_field_points_to_shot_and_field(self) -> None:
        bad = shot(0)
        del bad["momentum"]
        self.assert_invalid(
            record_payload(shots=[bad]),
            shot_index=0,
            field_part="shots[0].momentum",
        )

    def test_hit_outside_cover(self) -> None:
        self.assert_invalid(
            record_payload(shots=[shot(0, x=999)]),
            shot_index=0,
            field_part="hit_position",
        )

    def test_negative_angle_and_momentum(self) -> None:
        self.assert_invalid(
            record_payload(shots=[shot(0, angle=-0.1)]),
            shot_index=0,
            field_part="incidence_angle",
        )
        self.assert_invalid(
            record_payload(shots=[shot(0, momentum=-2)]),
            shot_index=0,
            field_part="momentum",
        )

    def test_declared_step_out_of_range(self) -> None:
        self.assert_invalid(
            record_payload(shots=[shot(0, step=99)]),
            shot_index=0,
            field_part="declared_step",
        )

    def test_unknown_cover_and_duplicate_block(self) -> None:
        self.assert_invalid(
            record_payload(shots=[shot(0, block_id=9)]),
            shot_index=0,
            field_part="block_id",
        )
        with self.assertRaises(RecordInvalid) as context:
            parse_record(record_payload(covers=[cover(1), cover(1)]), self.config)
        self.assertIn("block_id", context.exception.details["field"])

    def test_duplicate_client_sequence_rejected(self) -> None:
        shots = [shot(0, client_seq=7), shot(1, client_seq=7)]
        with self.assertRaises(RecordInvalid) as context:
            parse_record(record_payload(shots=shots), self.config)
        self.assertEqual(context.exception.details["shot_index"], 1)
        self.assertIn("client_seq", context.exception.details["field"])

    def test_config_invalid(self) -> None:
        with self.assertRaises(ConfigInvalid) as context:
            parse_config({"physics_steps": 0, "memory_budget_per_step": 1})
        self.assertEqual(context.exception.code, "E_CONFIG_INVALID")


if __name__ == "__main__":
    unittest.main()
