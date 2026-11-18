from __future__ import annotations

import copy
import json
import sqlite3
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

from duty_handover.core import build_snapshot, calculate_window, classify_burn
from duty_handover.data import Sample, SampleSet, load_handover, parse_timestamp
from duty_handover.errors import HandoverError
from duty_handover.generator import generate_all
from duty_handover.policy import load_policy
from duty_handover.service import build_service
from duty_handover.store import SnapshotStore, stable_json


UTC = timezone.utc


def sample(
    minute: int,
    *,
    service: str = "svc",
    total: int = 1000,
    success: int | None = None,
    p99: float = 100.0,
    target: float = 0.999,
    latency: float = 300.0,
) -> Sample:
    if success is None:
        success = total
    return Sample(
        timestamp=datetime(2026, 1, 1, tzinfo=UTC) + timedelta(minutes=minute),
        service=service,
        total_requests=total,
        success_requests=success,
        p99_latency_ms=p99,
        availability_target=target,
        latency_threshold_ms=latency,
    )


class CoreCalculationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.policy = load_policy()

    def test_latency_and_availability_union_consume_same_window(self) -> None:
        start = datetime(2026, 1, 1, tzinfo=UTC)
        rows = [
            sample(0, total=100),
            sample(1, total=100, p99=500.0),
            sample(2, total=100, success=90, p99=500.0),
        ]
        result = calculate_window(rows, start, start + timedelta(minutes=2))
        self.assertEqual(result["sample_count"], 3)
        self.assertAlmostEqual(result["budget_total"], 0.3)
        self.assertAlmostEqual(result["budget_consumed_availability"], 10.0)
        self.assertAlmostEqual(result["budget_consumed_latency_penalty"], 190.0)
        self.assertAlmostEqual(result["budget_consumed"], 200.0)
        self.assertAlmostEqual(result["budget_remaining"], -199.7)
        self.assertAlmostEqual(result["burn_rate"], 666.666667, places=6)
        self.assertAlmostEqual(result["availability_ratio"], 0.966667, places=6)
        self.assertEqual(result["availability_breach_minutes"], 1)
        self.assertEqual(result["latency_breach_minutes"], 2)
        self.assertEqual(result["latency_only_breach_minutes"], 1)
        self.assertTrue(result["availability_slo_met"] is False)
        self.assertTrue(result["latency_slo_met"] is False)

    def test_all_success_latency_breach_consumes_full_minute(self) -> None:
        start = datetime(2026, 1, 1, tzinfo=UTC)
        result = calculate_window(
            [sample(0, total=1000, success=1000, p99=301.0)],
            start,
            start,
        )
        self.assertEqual(result["availability_ratio"], 1.0)
        self.assertEqual(result["availability_breach_minutes"], 0)
        self.assertEqual(result["latency_breach_minutes"], 1)
        self.assertAlmostEqual(result["budget_consumed"], 1000.0)
        self.assertAlmostEqual(result["budget_total"], 1.0)
        self.assertAlmostEqual(result["burn_rate"], 1000.0)

    def test_burn_state_burst_and_sustained(self) -> None:
        self.assertEqual(classify_burn(10.0, 1.0, self.policy), "burst")
        self.assertEqual(classify_burn(3.0, 3.0, self.policy), "sustained")
        self.assertEqual(classify_burn(2.2, 2.0, self.policy), "sustained")
        self.assertEqual(classify_burn(0.5, 0.5, self.policy), "normal")

    def test_reading_order_does_not_change_results(self) -> None:
        rows = [sample(index, success=900 if index == 29 else 1000) for index in range(30)]
        forward = SampleSet(rows)
        reverse = SampleSet(reversed(rows))
        start = datetime(2026, 1, 1, tzinfo=UTC)
        end = start + timedelta(minutes=29)
        left = calculate_window(forward.window("svc", start, end), start, end)
        right = calculate_window(reverse.window("svc", start, end), start, end)
        self.assertEqual(stable_json(left), stable_json(right))


class GeneratedScenarioTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.data = self.root / "data"
        generate_all(self.data, seed=304)
        self.db = self.root / "state" / "handover.db"
        self.service = build_service(data_dir=self.data, db_path=self.db)
        self.policy = self.service.policy

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_generator_is_deterministic(self) -> None:
        second = self.root / "data2"
        first_manifest = generate_all(second, seed=304)
        original = json.loads((self.data / "manifest.json").read_text(encoding="utf-8"))
        self.assertEqual(original["samples_sha256"], first_manifest["samples_sha256"])
        self.assertEqual(
            (self.data / "samples.csv").read_bytes(),
            (second / "samples.csv").read_bytes(),
        )
        self.assertEqual(original["sample_rows"], 3 * 1440)

    def test_handover_snapshot_and_recompute(self) -> None:
        snapshot = self.service.decide()
        summary = snapshot["decision"]["summary"]
        self.assertEqual(summary["overall_grade"], "P2")
        self.assertEqual(summary["actual_grade"], "P2")
        self.assertEqual(summary["affected_services"], ["payment-gateway"])
        self.assertEqual(summary["burn_state"], "burst")
        self.assertGreater(summary["short_burn_rate"], summary["long_burn_rate"] * 2)
        self.assertEqual(
            snapshot["window_semantics"]["short_window"]["lookback_minutes"], 30
        )
        self.assertEqual(
            snapshot["window_semantics"]["long_window"]["lookback_minutes"], 360
        )
        self.assertEqual(summary["burn_state_basis"]["classification"], "burst")
        self.assertEqual(
            snapshot["services"]["payment-gateway"]["current_grade"], "P2"
        )
        recomputed = self.service.window(
            "payment-gateway",
            "2026-11-18T15:30:00Z",
            "2026-11-18T15:59:00Z",
        )
        self.assertEqual(recomputed["sample_count"], 30)
        self.assertEqual(recomputed["availability_breach_minutes"], 30)
        self.assertGreater(recomputed["burn_rate"], 249.0)
        self.assertLess(recomputed["burn_rate"], 251.0)
        self.assertAlmostEqual(recomputed["budget_consumed"], 7500.0, places=6)

    def test_latency_only_path_is_not_lowest_grade(self) -> None:
        handover = self.service.default_handover()
        handover["handover_at"] = "2026-11-18T16:49:00Z"
        snapshot = build_snapshot(handover, self.service.samples, self.policy)
        order = snapshot["services"]["order-api"]
        self.assertEqual(order["short_window"]["availability_ratio"], 0.999933)
        self.assertEqual(order["short_window"]["availability_breach_minutes"], 0)
        self.assertEqual(order["short_window"]["latency_breach_minutes"], 20)
        self.assertGreater(order["short_window"]["burn_rate"], 600.0)
        self.assertEqual(order["current_grade"], "P1")
        short_dimension = next(
            item
            for item in order["grading"]["dimensions"]
            if item["dimension"] == "short_burn_rate"
        )
        self.assertEqual(short_dimension["mapped_grade"], "P1")
        self.assertEqual(short_dimension["touched_threshold"]["threshold"], 14.4)

    def test_grading_has_values_for_every_dimension(self) -> None:
        snapshot = self.service.decide()
        grading = snapshot["decision"]["grading"]
        dimensions = {item["dimension"]: item for item in grading["dimensions"]}
        self.assertEqual(
            set(dimensions),
            {
                "error_budget_remaining_pct",
                "short_burn_rate",
                "long_burn_rate",
                "affected_services",
                "change_risk",
            },
        )
        for item in dimensions.values():
            self.assertIn("value", item)
            self.assertIn("mapped_grade", item)
        self.assertEqual(dimensions["change_risk"]["value"], "high")
        self.assertEqual(dimensions["change_risk"]["mapped_grade"], "P2")
        self.assertEqual(
            snapshot["services"]["payment-gateway"]["grading"]["grade"], "P2"
        )

    def test_silence_audit_and_metric_isolation(self) -> None:
        snapshot = self.service.decide()
        silence = snapshot["decision"]["silence"]
        pay_silence = next(
            item
            for item in silence["silences"]
            if item["silence_id"] == "SIL-2026-1118-PAY-01"
        )
        broad = next(
            item
            for item in silence["silences"]
            if item["silence_id"] == "SIL-2026-1118-GLOBAL-01"
        )
        self.assertEqual(pay_silence["status"], "ineffective")
        self.assertIn(
            "SILENCE_REMAINING_TOO_SHORT", pay_silence["blocking_reasons"]
        )
        self.assertEqual(broad["status"], "risk")
        self.assertIn("SILENCE_WILDCARD_SCOPE", broad["risk_reasons"])
        self.assertEqual(silence["decision"], "review")

        handover_without_silence = copy.deepcopy(self.service.default_handover())
        handover_without_silence["silences"] = []
        without = build_snapshot(
            handover_without_silence, self.service.samples, self.policy
        )
        for name, before in snapshot["services"].items():
            after = without["services"][name]
            self.assertEqual(
                before["error_budget_remaining_pct"],
                after["error_budget_remaining_pct"],
            )
            self.assertEqual(before["burn_rate_short"], after["burn_rate_short"])
            self.assertEqual(before["burn_rate_long"], after["burn_rate_long"])
            self.assertEqual(before["current_grade"], after["current_grade"])

    def test_duplicate_rejection_and_restart(self) -> None:
        first = self.service.submit()
        original = self.service.get_snapshot(first["handover_id"])
        with self.assertRaises(HandoverError) as timestamp_context:
            self.service.submit(
                load_handover(self.data / "handover_duplicate_timestamp.json")
            )
        self.assertEqual(
            timestamp_context.exception.code, "DUPLICATE_HANDOVER_TIMESTAMP"
        )
        self.assertEqual(self.service.store.snapshot_count(), 1)
        after = self.service.get_snapshot(first["handover_id"])
        self.assertEqual(stable_json(original), stable_json(after))
        rejection = self.service.store.latest_rejection()
        self.assertEqual(rejection["error_code"], "DUPLICATE_HANDOVER_TIMESTAMP")

        restarted_store = SnapshotStore(self.db)
        self.assertEqual(restarted_store.snapshot_count(), 1)
        self.assertEqual(
            restarted_store.get_snapshot(first["handover_id"])["snapshot"][
                "handover_id"
            ],
            first["handover_id"],
        )
        connection = sqlite3.connect(self.db)
        try:
            with self.assertRaises(sqlite3.DatabaseError):
                connection.execute(
                    "UPDATE snapshots SET handover_id = 'changed' WHERE snapshot_id = ?",
                    (first["snapshot_id"],),
                )
        finally:
            connection.close()

    def test_duplicate_handover_id_is_rejected(self) -> None:
        self.service.submit()
        with self.assertRaises(HandoverError) as context:
            self.service.submit(
                load_handover(self.data / "handover_duplicate_id.json")
            )
        self.assertEqual(context.exception.code, "DUPLICATE_HANDOVER_ID")
        self.assertEqual(
            self.service.store.latest_rejection()["error_code"],
            "DUPLICATE_HANDOVER_ID",
        )

    def test_replay_is_read_only_and_has_divergence(self) -> None:
        submitted = self.service.submit()
        before = self.service.get_snapshot(submitted["handover_id"])
        replay = self.service.replay(submitted["handover_id"])
        after = self.service.get_snapshot(submitted["handover_id"])
        self.assertEqual(stable_json(before), stable_json(after))
        self.assertEqual(replay["actual_grade"], "P2")
        self.assertEqual(replay["first_divergence"]["recommended_grade"], "P1")
        self.assertEqual(
            replay["first_divergence_at"], "2026-11-18T15:30:00Z"
        )
        self.assertIsNotNone(replay["first_divergence_trigger_metric"])
        self.assertEqual(
            len(replay["actual_series"]), len(replay["recommended_series"])
        )
        self.assertTrue(
            all(item["grade"] == "P2" for item in replay["actual_series"])
        )
        self.assertFalse(replay["historical_records_modified"])

    def test_replay_insufficient_samples_code(self) -> None:
        submitted = self.service.submit()
        with self.assertRaises(HandoverError) as context:
            self.service.replay(
                submitted["handover_id"], until="2026-11-19T00:00:00Z"
            )
        self.assertEqual(context.exception.code, "INSUFFICIENT_REPLAY_SAMPLES")
        self.assertEqual(context.exception.http_status, 422)

    def test_status_is_stable_sorted_and_path_free(self) -> None:
        submitted = self.service.submit()
        with self.assertRaises(HandoverError):
            self.service.submit(
                load_handover(self.data / "handover_duplicate_timestamp.json")
            )
        status = self.service.status(submitted["handover_id"])
        encoded = stable_json(status)
        self.assertEqual(status["snapshot"]["handover_id"], submitted["handover_id"])
        self.assertEqual(
            status["latest_rejected_submission"]["error_code"],
            "DUPLICATE_HANDOVER_TIMESTAMP",
        )
        self.assertEqual(
            status["replay"]["first_divergence_at"], "2026-11-18T15:30:00Z"
        )
        self.assertNotIn(str(self.root), encoded)
        self.assertNotIn("/home/", encoded)
        keys = list(json.loads(encoded).keys())
        self.assertEqual(keys, sorted(keys))

    def test_metrics_include_required_series(self) -> None:
        self.service.submit()
        text = self.service.metrics_text()
        for name in (
            "handover_error_budget_remaining_ratio",
            "handover_error_budget_remaining_pct",
            "handover_burn_rate_short",
            "handover_burn_rate_long",
            "handover_current_severity",
            "handover_silence_invalid_count",
            "handover_snapshot_count",
            "handover_snapshot_total",
        ):
            self.assertIn(name, text)
        self.assertIn('service="payment-gateway"', text)
        self.assertIn("handover_snapshot_total 1", text)

    def test_in_memory_sqlite_is_supported(self) -> None:
        memory_service = build_service(data_dir=self.data, db_path=":memory:")
        try:
            submitted = memory_service.submit()
            self.assertEqual(
                memory_service.get_snapshot(submitted["handover_id"])["handover_id"],
                submitted["handover_id"],
            )
            self.assertEqual(memory_service.store.snapshot_count(), 1)
        finally:
            memory_service.close()


if __name__ == "__main__":
    unittest.main()
