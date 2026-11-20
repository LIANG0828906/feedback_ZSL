from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from docagg.compare import refresh_summary, validate_against_reference
from docagg.config import EngineConfig, PipelineConfig
from docagg.errors import ErrorCode
from docagg.executor import run_aggregation
from docagg.reference import compute_reference


ROOT = Path(__file__).resolve().parents[1]
SAMPLE = ROOT / "samples" / "events.jsonl"
ANOMALIES = ROOT / "samples" / "anomalies.jsonl"
PIPELINE = ROOT / "samples" / "pipeline.json"


def load_pipeline() -> PipelineConfig:
    return PipelineConfig.from_dict(json.loads(PIPELINE.read_text(encoding="utf-8")))


def group_map(report: dict) -> dict[str, dict]:
    return {
        json.dumps(group["key"], sort_keys=True): group
        for group in report["groups"]
    }


class ExecutionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.pipeline = load_pipeline()
        cls.reference = compute_reference(SAMPLE, cls.pipeline, batch_size=4)

    def test_spill_and_non_spill_have_consistent_counts_and_bounds(self) -> None:
        no_spill = run_aggregation(
            SAMPLE,
            self.pipeline,
            EngineConfig(
                batch_size=3,
                spill_partitions=4,
            ),
        )
        spilled = run_aggregation(
            SAMPLE,
            self.pipeline,
            EngineConfig(
                batch_size=3,
                max_active_groups=2,
                max_state_bytes=1024,
                spill_partitions=3,
            ),
        )
        validate_against_reference(no_spill, self.reference)
        validate_against_reference(spilled, self.reference)

        self.assertEqual(len(no_spill["groups"]), len(spilled["groups"]))
        self.assertEqual(set(group_map(no_spill)), set(group_map(spilled)))
        self.assertGreater(spilled["spill"]["successful_spills"], 1)
        self.assertGreater(
            spilled["spill"]["group_occurrences_before_merge"],
            spilled["spill"]["distinct_groups_in_partials"],
        )
        self.assertTrue(spilled["spill"]["group_count_consistent"])
        self.assertEqual(spilled["summary"]["precision_error_count"], 0)

        no_spill_groups = group_map(no_spill)
        for group in spilled["groups"]:
            group_key = json.dumps(group["key"], sort_keys=True)
            for aggregate in group["aggregates"].values():
                self.assertEqual(aggregate["status"], "ok")
                self.assertTrue(
                    aggregate["reference_check"]["within_declared_error"]
                )
                self.assertEqual(
                    aggregate["error_estimate_basis"], "value_before_round"
                )
                self.assertIn("rounded_value", aggregate)
                self.assertIn("type_downgraded", aggregate)
                self.assertIn("source_types", aggregate)
                self.assertIn("decimal_precision_digits", aggregate)
                self.assertEqual(
                    aggregate["rounding"]["rounding_stage"],
                    "after_error_estimation",
                )
                self.assertGreaterEqual(
                    aggregate["post_round_error_estimate"],
                    aggregate["error_estimate"],
                )
                other = no_spill_groups[group_key]["aggregates"][
                    aggregate["name"]
                ]
                self.assertGreaterEqual(
                    aggregate["error_estimate"],
                    other["error_estimate"],
                )

    def test_cross_spill_scale_conflict_is_detected(self) -> None:
        rows = [
            {"g": 1, "amount": "1.20"},
            {"g": 2, "amount": 1.0},
            {"g": 2, "amount": 2.0},
            {"g": 1, "amount": "3.400"},
        ]
        pipeline = PipelineConfig.from_dict(
            {
                "group_by": ["g"],
                "aggregates": [{"name": "total", "op": "sum", "field": "amount"}],
            }
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "rows.jsonl"
            path.write_text(
                "\n".join(json.dumps(row) for row in rows) + "\n",
                encoding="utf-8",
            )
            report = run_aggregation(
                path,
                pipeline,
                EngineConfig(
                    batch_size=1,
                    max_active_groups=1,
                    spill_partitions=2,
                ),
            )
        self.assertTrue(
            any(
                error["code"] == ErrorCode.SCALE_MISMATCH.value
                for error in report["errors"]
            )
        )
        target = group_map(report)[json.dumps({"g": 1}, sort_keys=True)]
        self.assertEqual(target["aggregates"]["total"]["status"], "error")

    def test_non_finite_group_key_is_rejected(self) -> None:
        pipeline = PipelineConfig.from_dict(
            {
                "group_by": ["g"],
                "aggregates": [{"name": "total", "op": "sum", "field": "x"}],
            }
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "rows.jsonl"
            path.write_text('{"g":NaN,"x":1.0}\n', encoding="utf-8")
            report = run_aggregation(path, pipeline, EngineConfig(batch_size=2))
        self.assertEqual(report["groups"], [])
        self.assertEqual(report["errors"][0]["code"], ErrorCode.GROUP_KEY_INVALID.value)

    def test_spill_failure_is_explicit_and_results_are_preserved(self) -> None:
        report = run_aggregation(
            SAMPLE,
            self.pipeline,
            EngineConfig(
                batch_size=4,
                max_active_groups=1,
                fail_spill_on_attempt=1,
            ),
        )
        codes = {error["code"] for error in report["errors"]}
        self.assertIn(ErrorCode.SPILL_WRITE_FAILED.value, codes)
        self.assertEqual(len(report["groups"]), 5)
        self.assertTrue(report["spill"]["group_count_consistent"])
        self.assertEqual(report["spill"]["failed_spills"], 1)

    def test_invalid_spill_directory_is_a_spill_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            blocker = Path(directory) / "not-a-directory"
            blocker.write_text("blocked", encoding="utf-8")
            report = run_aggregation(
                SAMPLE,
                self.pipeline,
                EngineConfig(
                    batch_size=4,
                    max_active_groups=1,
                    spill_dir=str(blocker),
                ),
            )
        self.assertTrue(
            any(
                error["code"] == ErrorCode.SPILL_WRITE_FAILED.value
                for error in report["errors"]
            )
        )
        self.assertEqual(len(report["groups"]), 5)

    def test_anomaly_codes_and_completed_groups(self) -> None:
        report = run_aggregation(
            ANOMALIES,
            self.pipeline,
            EngineConfig(batch_size=4, max_active_groups=2),
        )
        codes = {error["code"] for error in report["errors"]}
        self.assertIn(ErrorCode.FIELD_MISSING.value, codes)
        self.assertIn(ErrorCode.TYPE_INCOMPARABLE.value, codes)
        self.assertIn(ErrorCode.SCALE_MISMATCH.value, codes)
        self.assertIn(ErrorCode.STDDEV_INSUFFICIENT_SAMPLES.value, codes)

        groups = group_map(report)
        good = groups[json.dumps(
            {"bucket": 1, "region": "north", "tenant": "good"},
            sort_keys=True,
        )]
        self.assertEqual(good["aggregates"]["amount_sum"]["status"], "ok")

        missing = groups[json.dumps(
            {"bucket": 3, "region": "missing", "tenant": "bad"},
            sort_keys=True,
        )]
        self.assertIn(
            ErrorCode.STDDEV_INSUFFICIENT_SAMPLES.value,
            missing["aggregates"]["amount_stddev"]["error_codes"],
        )

        solo = groups[json.dumps(
            {"bucket": 6, "region": "west", "tenant": "solo"},
            sort_keys=True,
        )]
        self.assertEqual(solo["aggregates"]["amount_sum"]["status"], "ok")
        self.assertEqual(
            solo["aggregates"]["amount_stddev"]["error_codes"],
            [ErrorCode.STDDEV_INSUFFICIENT_SAMPLES.value],
        )

    def test_precision_bound_failure_emits_code(self) -> None:
        report = run_aggregation(SAMPLE, self.pipeline, EngineConfig(batch_size=4))
        report["groups"][0]["aggregates"]["amount_sum"]["value"] += 1e20
        issues = validate_against_reference(report, self.reference)
        self.assertTrue(
            any(issue.code == ErrorCode.PRECISION_BOUND for issue in issues)
        )
        self.assertGreater(report["summary"]["precision_error_count"], 0)

    def test_repeated_runs_are_deterministic(self) -> None:
        config = EngineConfig(
            batch_size=3,
            max_active_groups=2,
            spill_partitions=3,
        )
        first = run_aggregation(SAMPLE, self.pipeline, config)
        second = run_aggregation(SAMPLE, self.pipeline, config)
        refresh_summary(first)
        refresh_summary(second)
        self.assertEqual(first["groups"], second["groups"])
        self.assertEqual(first["errors"], second["errors"])
        self.assertEqual(
            first["summary"]["deterministic_digest"],
            second["summary"]["deterministic_digest"],
        )

    def test_reference_uses_rational_and_high_precision_methods(self) -> None:
        methods = {
            aggregate["method"]
            for group in self.reference["groups"]
            for aggregate in group["aggregates"].values()
            if aggregate.get("status") == "ok"
        }
        self.assertIn("high_precision_decimal_simulation", methods)
        self.assertIn("exact_rational", methods)


if __name__ == "__main__":
    unittest.main()
