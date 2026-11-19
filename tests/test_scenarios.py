from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from settlement_rollout.scenario import run_scenario

from .helpers import (
    QUICK_PLAN,
    QUICK_SLO,
    REFUSAL_INJECTION,
    ROLLBACK_INJECTION,
    SUCCESS_INJECTION,
    normalize_timings,
)


class ScenarioTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _run(self, injection: Path, name: str):
        workdir = self.directory / name
        report_path = self.directory / f"{name}.json"
        report, exit_code = run_scenario(
            QUICK_PLAN,
            QUICK_SLO,
            injection,
            workdir,
            report_path,
            reset=True,
        )
        self.assertEqual(exit_code, 0)
        self.assertEqual(report, json.loads(report_path.read_text(encoding="utf-8")))
        return report, workdir

    def test_successful_progression(self) -> None:
        report, _ = self._run(SUCCESS_INJECTION, "success")
        self.assertEqual(report["outcome"], "RELEASED")
        self.assertEqual(
            [batch["status"] for batch in report["batches"]],
            ["PASSED", "PASSED"],
        )
        for batch in report["batches"]:
            self.assertEqual(batch["decision"]["verdict"], "PASS")
            self.assertEqual(batch["sli"]["sample_count"], 12)
            self.assertEqual(batch["sli"]["p99_latency_ms"], 25.0)
            self.assertEqual(batch["sli"]["availability_percent"], 100.0)
            self.assertEqual(batch["sli"]["error_budget_remaining_ratio"], 1.0)
        self.assertEqual(report["rollback_sequence"], [])
        self.assertEqual(report["transport"]["connection_errors"], 0)

    def test_failed_batch_rolls_back_without_advancing(self) -> None:
        report, _ = self._run(ROLLBACK_INJECTION, "rollback")
        self.assertEqual(report["outcome"], "ROLLED_BACK")
        first, second = report["batches"]
        self.assertEqual(first["status"], "ROLLED_BACK")
        self.assertEqual(second["status"], "PENDING")
        self.assertEqual(first["decision"]["verdict"], "FAIL")
        triggers = {item["code"] for item in first["decision"]["triggers"]}
        self.assertIn("P99_ABOVE_TARGET", triggers)
        self.assertIn("ERROR_BUDGET_EXCEEDED", triggers)
        self.assertEqual(len(first["rollback"]["actions"]), 3)
        self.assertEqual(first["remaining_switched_instances"], [])
        self.assertIsNotNone(first["rollback"]["duration_ms"])
        self.assertEqual(report["transport"]["connection_errors"], 0)

    def test_precheck_skip_and_restart_refusal_are_auditable(self) -> None:
        report, workdir = self._run(REFUSAL_INJECTION, "refusal")
        self.assertEqual(report["outcome"], "PARTIAL_ROLLBACK")
        first = report["batches"][0]
        self.assertEqual(
            first["precheck"]["failed_instances"][0]["instance_id"],
            "site-a-1",
        )
        self.assertEqual(
            first["precheck"]["failed_instances"][0]["code"],
            "INSTANCE_PRECHECK_FAILED",
        )
        refused = [
            action
            for action in first["rollback"]["actions"]
            if action["error_code"] == "INSTANCE_RESTART_REFUSED"
        ]
        self.assertEqual(len(refused), 1)
        self.assertEqual(refused[0]["instance_id"], "site-a-2")
        self.assertEqual(first["remaining_switched_instances"], ["site-a-2"])
        self.assertEqual(report["batches"][1]["status"], "PENDING")

        import sqlite3

        connection = sqlite3.connect(workdir / "state.sqlite3")
        connection.row_factory = sqlite3.Row
        try:
            decision = connection.execute(
                "SELECT * FROM decisions WHERE batch_id = ?",
                ("canary-site-a",),
            ).fetchone()
            self.assertIsNotNone(decision)
            snapshot = connection.execute(
                "SELECT * FROM metrics_snapshots "
                "WHERE scope = 'DECISION' AND batch_id = ?",
                ("canary-site-a",),
            ).fetchone()
            self.assertIsNotNone(snapshot)
            event = connection.execute(
                "SELECT * FROM events "
                "WHERE event_type = 'DECISION_RECORDED' AND batch_id = ?",
                ("canary-site-a",),
            ).fetchone()
            self.assertIsNotNone(event)
        finally:
            connection.close()

    def test_report_is_reproducible_except_timings(self) -> None:
        first, _ = self._run(ROLLBACK_INJECTION, "repeat-a")
        second, _ = self._run(ROLLBACK_INJECTION, "repeat-b")
        self.assertEqual(
            normalize_timings(first),
            normalize_timings(second),
        )


if __name__ == "__main__":
    unittest.main()
