from __future__ import annotations

import json
import sqlite3
import tempfile
import unittest
from pathlib import Path

from settlement.engine import ExecutionConfig, Executor
from settlement.errors import ExecutionStopped, MemoryLimitError, SettlementError
from settlement.generator import GeneratorConfig, generate_dataset
from settlement.sql import SqlParseError, parse_sql


ROOT = Path(__file__).resolve().parents[1]
SQL_PATH = ROOT / "examples" / "settlement.sql"


class SmallDatasetTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory(prefix="settlement-tests-")
        cls.root = Path(cls._temporary.name)
        cls.data = cls.root / "data"
        generate_dataset(
            cls.data,
            GeneratorConfig(
                seed=424242,
                days=1,
                telemetry_rows=2400,
                supplement_rows=900,
                batches=8,
                vehicle_count=30,
            ),
            overwrite=True,
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def config(self, name: str, **overrides) -> ExecutionConfig:
        values = {
            "sql": SQL_PATH,
            "data_dir": self.data,
            "db_path": self.root / f"{name}.db",
            "mode": "pushdown",
            "slots": 5,
            "temp_dir": self.root / f"{name}-tmp",
        }
        values.update(overrides)
        return ExecutionConfig(**values)

    def test_sql_safety_contract(self) -> None:
        query = parse_sql(SQL_PATH.read_text(encoding="utf-8"))
        self.assertEqual(query.window_seconds, 3600)
        self.assertTrue(any(item.field == "partition_date" for item in query.pushed_predicates))
        self.assertTrue(any(item.field == "event_time" for item in query.retained_predicates))
        self.assertFalse(any(item.field == "event_time" for item in query.pushed_predicates))
        plan = query.plan_summary()
        self.assertEqual(plan["join_keys"], ["vehicle_id", "trip_id"])
        self.assertEqual(plan["aliases"]["energy_delta"], "energy_delta")
        with self.assertRaises(SqlParseError):
            parse_sql(
                "SELECT vehicle_id,date_trunc('hour',event_time) AS w "
                "FROM vehicle_events WHERE vehicle_id='V1' OR trip_id='T1' "
                "GROUP BY vehicle_id,date_trunc('hour',event_time)"
            )

    def test_like_predicate(self) -> None:
        query = parse_sql(
            "SELECT vehicle_id,date_trunc('hour',event_time) AS window_start "
            "FROM vehicle_events WHERE vehicle_id LIKE 'V000%' "
            "GROUP BY vehicle_id,date_trunc('hour',event_time)"
        )
        predicate = query.pushed_predicates[0]
        self.assertTrue(predicate.matches({"vehicle_id": "V0001"}))
        self.assertFalse(predicate.matches({"vehicle_id": "X0001"}))

    def test_pushdown_and_replay_are_exactly_equal(self) -> None:
        results = Executor(self.config("both", mode="both")).run()
        self.assertEqual([item.status for item in results], ["published", "published"])
        verification = results[-1].report["verification"]
        self.assertTrue(verification["equivalent"])
        self.assertEqual(verification["mismatched_windows"], 0)
        self.assertEqual(verification["mismatched_vehicles"], 0)
        self.assertEqual(verification["max_mileage_diff_micro_units"], 0)
        self.assertEqual(verification["max_energy_diff_micro_units"], 0)
        self.assertEqual(verification["max_vehicle_energy_per_100km_diff"], "0")
        self.assertTrue(verification["late"]["counts_match"])
        self.assertGreater(verification["late"]["pushdown_count"], 0)
        self.assertGreater(verification["dedup"]["pushdown_count"], 0)
        for result in results:
            self.assertLessEqual(result.report["slots"]["max_share"], 0.4)
            self.assertTrue(result.report["skew"]["vehicles"])
            self.assertGreaterEqual(result.report["inputs"]["telemetry_rows"], 2000)
        connection = sqlite3.connect(self.root / "both.db")
        try:
            duplicate_count = connection.execute(
                "SELECT COUNT(*) FROM ("
                "SELECT run_id,vehicle_id,window_start_us,COUNT(*) AS c "
                "FROM vehicle_energy GROUP BY run_id,vehicle_id,window_start_us "
                "HAVING c>1)"
            ).fetchone()[0]
            total_count = connection.execute(
                "SELECT COUNT(*) FROM vehicle_energy_total"
            ).fetchone()[0]
        finally:
            connection.close()
        self.assertEqual(duplicate_count, 0)
        self.assertEqual(total_count, 60)

    def test_resume_matches_uninterrupted_run(self) -> None:
        Executor(self.config("fresh")).run()
        with self.assertRaises(ExecutionStopped):
            Executor(self.config("resumed", stop_after_batch=3)).run()
        connection = sqlite3.connect(self.root / "resumed.db")
        try:
            self.assertEqual(
                connection.execute("SELECT COUNT(*) FROM vehicle_energy").fetchone()[0],
                0,
            )
            self.assertGreater(
                connection.execute("SELECT COUNT(*) FROM stage_vehicle_energy").fetchone()[0],
                0,
            )
        finally:
            connection.close()
        resumed = Executor(self.config("resumed")).run()[0]
        self.assertEqual(resumed.report["checkpoints"]["resume_count"], 1)
        self.assertEqual(resumed.report["checkpoints"]["resume_points"][0]["consumed_batch"], 3)
        self.assertEqual(
            _vehicle_totals(self.root / "fresh.db"),
            _vehicle_totals(self.root / "resumed.db"),
        )

    def test_failed_batch_is_invisible_and_retry_is_unique(self) -> None:
        with self.assertRaises(SettlementError) as raised:
            Executor(self.config("failed", fail_batch=4)).run()
        self.assertEqual(raised.exception.code, "E_INJECTED_BATCH_FAILURE")
        connection = sqlite3.connect(self.root / "failed.db")
        connection.row_factory = sqlite3.Row
        try:
            self.assertEqual(
                connection.execute("SELECT COUNT(*) FROM vehicle_energy").fetchone()[0],
                0,
            )
            failed = connection.execute(
                "SELECT status,error_code FROM batch_attempts WHERE batch_id=4"
            ).fetchone()
            self.assertEqual(failed["status"], "failed")
            stage_before = connection.execute(
                "SELECT COUNT(*) FROM stage_vehicle_energy"
            ).fetchone()[0]
            self.assertGreater(stage_before, 0)
        finally:
            connection.close()
        Executor(self.config("failed", fail_batch=4)).run()
        connection = sqlite3.connect(self.root / "failed.db")
        try:
            attempts = connection.execute(
                "SELECT status FROM batch_attempts WHERE batch_id=4 ORDER BY attempt_no"
            ).fetchall()
            self.assertEqual([row[0] for row in attempts], ["failed", "success"])
            self.assertGreater(
                connection.execute(
                    "SELECT COUNT(*) FROM stage_vehicle_energy WHERE plan_mode='pushdown'"
                ).fetchone()[0],
                stage_before,
            )
            duplicate_count = connection.execute(
                "SELECT COUNT(*) FROM ("
                "SELECT vehicle_id,window_start_us,COUNT(*) AS c FROM vehicle_energy "
                "GROUP BY vehicle_id,window_start_us HAVING c>1)"
            ).fetchone()[0]
            self.assertEqual(duplicate_count, 0)
        finally:
            connection.close()

    def test_spill_is_accounted_and_cleaned(self) -> None:
        result = Executor(
            self.config("spill", spill_threshold_bytes=1)
        ).run()[0]
        self.assertGreater(result.report["spill"]["bytes"], 0)
        self.assertTrue(result.report["spill"]["temporary_directory_removed"])
        self.assertFalse((self.root / "spill-tmp").exists())

    def test_memory_limit_uses_stable_error(self) -> None:
        with self.assertRaises(MemoryLimitError) as raised:
            Executor(self.config("memory", memory_limit_bytes=1024 * 1024)).run()
        self.assertEqual(raised.exception.code, "E_MEMORY")

    def test_stable_report_hash_on_repeat(self) -> None:
        first = Executor(self.config("stable-a")).run()[0]
        second = Executor(self.config("stable-b")).run()[0]
        self.assertEqual(first.report["stable_hash"], second.report["stable_hash"])
        for report in (first.report, second.report):
            report.pop("stable_hash", None)
            report.pop("runtime", None)
            report["memory"].pop("runtime_peak_rss_bytes", None)
            report["memory"].pop("python_peak_bytes", None)
        self.assertEqual(first.report, second.report)


def _vehicle_totals(db_path: Path) -> list[tuple]:
    connection = sqlite3.connect(db_path)
    try:
        return connection.execute(
            "SELECT vehicle_id,mileage_delta,energy_delta,energy_per_100km_text,"
            "event_count,telemetry_count,supplement_count,window_count "
            "FROM vehicle_energy_total WHERE plan_mode='pushdown' ORDER BY vehicle_id"
        ).fetchall()
    finally:
        connection.close()


if __name__ == "__main__":
    unittest.main()

