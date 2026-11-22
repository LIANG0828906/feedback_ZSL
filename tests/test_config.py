from __future__ import annotations

import unittest

from volrender.config import load_config, parse_config
from volrender.errors import BusinessError, ErrorCode

from tests.helpers import config


class ConfigTests(unittest.TestCase):
    def test_parse_and_hash_are_stable(self) -> None:
        a = config()
        b = config()
        self.assertEqual(a.content_hash, b.content_hash)
        self.assertEqual(a.max_parallelism, 2)
        self.assertEqual(a.sample_budget, 500_000)

    def test_over_budget_configuration_is_rejected(self) -> None:
        with self.assertRaises(BusinessError) as caught:
            config(sample_budget=50_000_001)
        self.assertEqual(caught.exception.code, ErrorCode.CONFIG_BUDGET)
        self.assertEqual(caught.exception.field, "sample_budget")

    def test_injected_budget_has_own_code_and_offset(self) -> None:
        with self.assertRaises(BusinessError) as caught:
            load_config(
                b'{"tile_size": 8, "max_parallelism": 1, "sample_budget": 10, '
                b'"base_step": 0.1, "min_step": 0.01, "precision": "float", '
                b'"degrade_sequence": [{"name": "S0", "step_multiplier": 1, '
                b'"shadow_samples": 1}], "seam": {"enabled": false, '
                b'"tolerance": 0, "truth_budget": 100}, "limits": '
                b'{"max_parallelism": 2, "max_sample_budget": 100}}',
                inject="config_budget",
            )
        self.assertEqual(caught.exception.code, ErrorCode.CONFIG_BUDGET)
        self.assertEqual(caught.exception.field, "sample_budget")
        self.assertGreaterEqual(caught.exception.offset, 0)

    def test_parallelism_limit_is_enforced(self) -> None:
        with self.assertRaises(BusinessError) as caught:
            config(
                max_parallelism=3,
                limits={"max_parallelism": 2, "max_sample_budget": 50_000_000},
            )
        self.assertEqual(caught.exception.code, ErrorCode.CONFIG_PARALLELISM)

    def test_degrade_sequence_must_be_monotonic(self) -> None:
        raw = config().to_dict()
        raw["degrade_sequence"][1]["step_multiplier"] = 0.5
        with self.assertRaises(BusinessError) as caught:
            parse_config(__import__("json").dumps(raw))
        self.assertEqual(caught.exception.code, ErrorCode.CONFIG_INVALID)


if __name__ == "__main__":
    unittest.main()
