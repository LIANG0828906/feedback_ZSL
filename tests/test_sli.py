from __future__ import annotations

import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from settlement_rollout.config import load_configuration
from settlement_rollout.sli import Sample, evaluate_samples

from .helpers import QUICK_PLAN, QUICK_SLO


class SLIEvaluationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        base = Path(cls.temporary.name)
        cls.config = load_configuration(QUICK_PLAN, QUICK_SLO)
        cls.config = replace(
            cls.config,
            storage=replace(
                cls.config.storage,
                database_path=base / "state.sqlite3",
                report_path=base / "report.json",
            ),
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def test_insufficient_samples_never_passes(self) -> None:
        result = evaluate_samples(self.config, [], ["site-a-1"])
        self.assertEqual(result["verdict"], "FAIL")
        self.assertEqual(result["triggers"][0]["code"], "INSUFFICIENT_SAMPLES")

    def test_p99_fails_even_when_budget_cap_is_not_reached(self) -> None:
        config = replace(
            self.config,
            sampling=replace(
                self.config.sampling,
                quota_per_instance=100,
            ),
            slo=replace(
                self.config.slo,
                latency_allowed_bad_percent=5.0,
                max_error_budget_consumed_percent=1000.0,
                minimum_samples=10,
            ),
        )
        samples = [
            Sample("site-a-1", index, 200, 10.0, "v2")
            for index in range(1, 99)
        ]
        samples.extend(
            [
                Sample("site-a-1", 99, 200, 1000.0, "v2"),
                Sample("site-a-1", 100, 200, 1000.0, "v2"),
            ]
        )
        result = evaluate_samples(config, samples, ["site-a-1"])
        codes = [item["code"] for item in result["triggers"]]
        self.assertIn("P99_ABOVE_TARGET", codes)
        self.assertNotIn("ERROR_BUDGET_EXCEEDED", codes)
        self.assertGreater(
            result["metrics"]["error_budget_remaining_ratio"], 0.0
        )

    def test_5xx_consumes_availability_budget(self) -> None:
        config = replace(
            self.config,
            sampling=replace(
                self.config.sampling,
                quota_per_instance=20,
            ),
        )
        samples = [
            Sample("site-a-1", index, 500 if index % 10 == 0 else 200, 20.0, "v2")
            for index in range(1, 21)
        ]
        result = evaluate_samples(config, samples, ["site-a-1"])
        self.assertEqual(result["verdict"], "FAIL")
        self.assertLess(
            result["metrics"]["availability_percent"],
            config.slo.availability_target_percent,
        )
        self.assertIn(
            "AVAILABILITY_BELOW_TARGET",
            [item["code"] for item in result["triggers"]],
        )


if __name__ == "__main__":
    unittest.main()
