#!/usr/bin/env python3
"""End-to-end acceptance checks using only the Python standard library."""

from __future__ import annotations

import concurrent.futures
import json
import os
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import traceback
import urllib.error
import urllib.request
from pathlib import Path

from generate_metadata import generate_database


ROOT = Path(__file__).resolve().parent
HOST = "127.0.0.1"
PORT = 8010
BASE_URL = f"http://{HOST}:{PORT}"


def request(
    method: str,
    path: str,
    body: dict | None = None,
    timeout: float = 15,
) -> tuple[int, dict]:
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(
        BASE_URL + path, data=data, headers=headers, method=method
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            return response.status, json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read().decode("utf-8"))


def port_free(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        try:
            sock.bind((HOST, port))
            return True
        except OSError:
            return False


class RunningService:
    def __init__(self, db_path: Path, log_path: Path) -> None:
        self.db_path = db_path
        self.log_path = log_path
        self.process: subprocess.Popen | None = None

    def start(self) -> None:
        log = open(self.log_path, "ab")
        self.process = subprocess.Popen(
            [
                sys.executable,
                str(ROOT / "lineage_service.py"),
                "--db",
                str(self.db_path),
                "--host",
                HOST,
                "--port",
                str(PORT),
                "--log-level",
                "WARNING",
            ],
            cwd=ROOT,
            stdout=log,
            stderr=log,
        )
        log.close()
        deadline = time.time() + 15
        last_error: Exception | None = None
        while time.time() < deadline:
            if self.process.poll() is not None:
                raise AssertionError("service process exited during startup")
            try:
                request("GET", "/health", timeout=1)
                return
            except Exception as error:  # startup race
                last_error = error
                time.sleep(0.1)
        raise AssertionError(f"service did not start: {last_error}")

    def stop(self) -> None:
        if self.process is None:
            return
        self.process.terminate()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)
        self.process = None


def assert_true(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def run_acceptance() -> None:
    if not port_free(PORT):
        raise AssertionError(f"port {PORT} is already in use")
    temporary = Path(tempfile.mkdtemp(prefix="warehouse-impact-"))
    db_path = temporary / "metadata.db"
    bad_db_path = temporary / "bad_metadata.db"
    cycle_db_path = temporary / "cycle_metadata.db"
    log_path = temporary / "service.log"
    generate_database(db_path, orders=360)
    service = RunningService(db_path, log_path)
    try:
        service.start()
        status, health = request("GET", "/health")
        assert_true(status == 200 and health["status"] == "READY", "health")
        assert_true(health["counts"]["tables"] == 12, "12 tables")
        assert_true(health["counts"]["tasks"] == 40, "40 tasks")
        assert_true(health["counts"]["metrics"] == 8, "8 metrics")

        change = {
            "table_name": "detail_order_items",
            "column_name": "unit_price",
            "change_type": "type_narrow",
            "new_type": "TEXT(2)",
            "threshold_pct": 0.5,
        }
        status, impact = request("POST", "/api/changes", change)
        assert_true(status == 200 and impact["ok"], "submit change")
        change_id = impact["change_id"]
        summary = impact["summary"]
        assert_true(
            summary["affected_task_count"]
            == summary["direct_count"] + summary["transitive_count"],
            "direct/transitive partition",
        )
        assert_true(summary["direct_count"] >= 1, "direct impact")
        assert_true(summary["transitive_count"] >= 1, "transitive impact")
        task_names = [
            item["task_name"] for item in impact["affected_tasks"]
        ]
        assert_true(len(task_names) == len(set(task_names)), "no duplicates")
        assert_true(
            all(
                item["impact_type"] in {"direct", "transitive"}
                for item in impact["affected_tasks"]
            ),
            "impact classification",
        )
        assert_true(
            len({item["report_id"] for item in impact["affected_reports"]})
            == 7,
            "all configured reports affected",
        )

        status, duplicate = request("POST", "/api/changes", change)
        assert_true(status == 200, "duplicate status")
        assert_true(duplicate["change_id"] == change_id, "idempotent id")
        assert_true(
            duplicate["summary"] == summary, "idempotent impact"
        )
        status, fetched = request(
            "GET", f"/api/changes/{change_id}/impact"
        )
        assert_true(
            status == 200 and fetched["summary"] == summary,
            "persisted impact",
        )

        status, drift = request(
            "POST", f"/api/changes/{change_id}/drift-check", {}
        )
        assert_true(status == 200 and drift["status"] == "E_DRIFT", "drift")
        assert_true(len(drift["metric_results"]) == 8, "8 metrics checked")
        assert_true(
            any(
                metric["relative_deviation_pct"] > 0
                for metric in drift["metric_results"]
            ),
            "non-zero drift",
        )
        assert_true(
            all(
                metric["first_drift_table"] == "detail_order_items"
                for metric in drift["metric_results"]
                if metric["status"] == "E_DRIFT"
            ),
            "first drift table",
        )
        status, drift_again = request(
            "POST", f"/api/changes/{change_id}/drift-check", {}
        )
        assert_true(
            status == 200
            and drift_again["drift_check_id"]
            == drift["drift_check_id"],
            "drift idempotency",
        )

        status, rollback = request(
            "GET", f"/api/changes/{change_id}/rollback-plan"
        )
        assert_true(status == 200 and rollback["blocked"] is False, "rollback")
        assert_true(
            len(rollback["rollback_tasks"]) == summary["affected_task_count"],
            "complete rollback list",
        )
        assert_true(
            len(rollback["recheck_metrics"]) == 8,
            "all summary metrics must be rechecked",
        )
        depths = [
            item["dependency_depth"] for item in rollback["rollback_tasks"]
        ]
        assert_true(depths == sorted(depths, reverse=True), "reverse depth")

        status, blocked = request(
            "POST",
            "/api/changes",
            {
                "table_name": "detail_order_items",
                "column_name": "unit_price",
                "change_type": "delete_column",
            },
        )
        assert_true(status == 200, "delete change")
        delete_id = blocked["change_id"]
        status, blocked_rollback = request(
            "GET", f"/api/changes/{delete_id}/rollback-plan"
        )
        assert_true(
            status == 409
            and blocked_rollback["code"] == "E_ROLLBACK_BLOCKED",
            "rollback blocked",
        )
        assert_true(
            blocked_rollback["details"]["rollback_tasks"] == [],
            "no incomplete rollback list",
        )

        status, invalid = request(
            "POST",
            "/api/changes",
            {
                "table_name": "detail_order_items",
                "column_name": "unit_price",
                "change_type": "delete_column",
                "threshold_pct": 100.1,
            },
        )
        assert_true(
            status == 400 and invalid["code"] == "E_THRESHOLD_RANGE",
            "threshold validation",
        )
        for body, code in [
            (
                {
                    "table_name": "unknown_table",
                    "column_name": "x",
                    "change_type": "delete_column",
                },
                "E_UNKNOWN_TABLE",
            ),
            (
                {
                    "table_name": "detail_order_items",
                    "column_name": "unknown_column",
                    "change_type": "delete_column",
                },
                "E_UNKNOWN_COLUMN",
            ),
            (
                {
                    "table_name": "detail_order_items",
                    "column_name": "unit_price",
                    "change_type": "shrink",
                },
                "E_INVALID_CHANGE_TYPE",
            ),
            (
                {
                    "task_name": "unknown_task",
                    "table_name": "detail_order_items",
                    "column_name": "unit_price",
                    "change_type": "delete_column",
                },
                "E_UNKNOWN_TASK",
            ),
        ]:
            status, body_result = request("POST", "/api/changes", body)
            assert_true(
                status == 400 and body_result["code"] == code,
                f"business error {code}",
            )

        def submit_once(_: int) -> tuple[int, str]:
            status, result = request("POST", "/api/changes", change)
            return status, result["change_id"]

        with concurrent.futures.ThreadPoolExecutor(
            max_workers=6
        ) as executor:
            concurrent_results = list(
                executor.map(submit_once, range(12))
            )
        assert_true(
            all(status == 200 for status, _ in concurrent_results),
            "concurrent submissions",
        )
        assert_true(
            {result_id for _, result_id in concurrent_results}
            == {change_id},
            "concurrent idempotency",
        )

        status, audit = request("GET", f"/api/changes/{change_id}/audit")
        assert_true(status == 200, "audit")
        assert_true(len(audit["impact_history"]) == 1, "impact not overwritten")
        assert_true(len(audit["drift_history"]) == 1, "drift history")
        assert_true(
            any(
                event["event_type"] == "change_resubmitted"
                for event in audit["audit_events"]
            ),
            "audit resubmissions",
        )

        service.stop()
        service.start()
        status, audit_after_restart = request(
            "GET", f"/api/changes/{change_id}/audit"
        )
        assert_true(
            status == 200
            and audit_after_restart["change"]["change_id"] == change_id,
            "restart persistence",
        )
        assert_true(
            len(audit_after_restart["impact_history"]) == 1,
            "impact survives restart",
        )
        service.stop()

        shutil.copyfile(db_path, bad_db_path)
        conn = sqlite3.connect(bad_db_path)
        try:
            conn.execute(
                "UPDATE tasks SET task_sql = ? WHERE task_name = ?",
                (
                    "UPDATE detail_orders SET gross_amount = no_such_column "
                    "WHERE 0;",
                    "detail_orders_net_refresh",
                ),
            )
            conn.commit()
        finally:
            conn.close()
        bad_service = RunningService(bad_db_path, log_path)
        bad_service.start()
        try:
            status, bad_health = request("GET", "/health")
            assert_true(
                status == 400 and bad_health["code"] == "E_UNKNOWN_REF",
                "unknown ref health",
            )
            details = bad_health["details"]
            assert_true(
                details["task_name"] == "detail_orders_net_refresh",
                "unknown ref task",
            )
            assert_true(
                details["missing_object"] == "no_such_column",
                "unknown ref object",
            )
            status, bad_submit = request(
                "POST",
                "/api/changes",
                {
                    "table_name": "detail_order_items",
                    "column_name": "unit_price",
                    "change_type": "delete_column",
                },
            )
            assert_true(
                status == 400 and bad_submit["code"] == "E_UNKNOWN_REF",
                "unknown ref blocks service",
            )
        finally:
            bad_service.stop()

        shutil.copyfile(db_path, cycle_db_path)
        conn = sqlite3.connect(cycle_db_path)
        try:
            conn.execute(
                "UPDATE tasks SET task_sql = ? WHERE task_name = ?",
                (
                    "UPDATE detail_orders SET net_amount = ROUND("
                    "gross_amount - discount_amount + tax_amount, 2) "
                    "WHERE (SELECT COUNT(*) FROM summary_finance) >= 0;",
                    "detail_orders_net_refresh",
                ),
            )
            conn.execute(
                "UPDATE tasks SET task_sql = ? WHERE task_name = ?",
                (
                    "UPDATE summary_finance SET net_sales = ROUND("
                    "gross_sales - discount_amount + tax_amount, 2) "
                    "WHERE (SELECT COUNT(*) FROM detail_orders) >= 0;",
                    "summary_finance_publish",
                ),
            )
            conn.commit()
        finally:
            conn.close()
        cycle_service = RunningService(cycle_db_path, log_path)
        cycle_service.start()
        try:
            status, cycle_change = request(
                "POST",
                "/api/changes",
                {
                    "table_name": "detail_order_items",
                    "column_name": "unit_price",
                    "change_type": "type_narrow",
                    "new_type": "INTEGER",
                },
            )
            assert_true(status == 200, "cycle fixture change")
            status, cycle_plan = request(
                "GET",
                f"/api/changes/{cycle_change['change_id']}/rollback-plan",
            )
            assert_true(
                status == 409
                and cycle_plan["code"] == "E_ROLLBACK_BLOCKED",
                "cycle rollback blocked",
            )
            assert_true(
                any(
                    reason["reason"] == "TASK_DEPENDENCY_CYCLE"
                    for reason in cycle_plan["details"]["blocking_reasons"]
                ),
                "cycle blocking reason",
            )
            assert_true(
                cycle_plan["details"]["rollback_tasks"] == [],
                "cycle emits no partial rollback list",
            )
        finally:
            cycle_service.stop()

        conn = sqlite3.connect(db_path)
        try:
            counts = {
                table: conn.execute(
                    f"SELECT COUNT(*) FROM {table}"
                ).fetchone()[0]
                for table in (
                    "change_requests",
                    "impact_results",
                    "drift_checks",
                    "rollback_plans",
                    "audit_events",
                )
            }
        finally:
            conn.close()
        assert_true(counts["change_requests"] >= 2, "changes persisted")
        assert_true(counts["impact_results"] >= 2, "impacts persisted")
        assert_true(counts["drift_checks"] == 1, "drift persisted")
        assert_true(counts["rollback_plans"] >= 2, "rollback persisted")
        assert_true(counts["audit_events"] >= 5, "audit persisted")
        print(
            json.dumps(
                {
                    "status": "PASS",
                    "change_id": change_id,
                    "affected_tasks": summary["affected_task_count"],
                    "direct": summary["direct_count"],
                    "transitive": summary["transitive_count"],
                    "drifted_metrics": drift["drifted_metrics"],
                    "db_counts": counts,
                },
                ensure_ascii=False,
                indent=2,
            )
        )
    except Exception:
        print(traceback.format_exc(), file=sys.stderr)
        if log_path.exists():
            print("--- service log ---", file=sys.stderr)
            print(log_path.read_text(errors="replace"), file=sys.stderr)
        raise
    finally:
        service.stop()
        shutil.rmtree(temporary, ignore_errors=True)


if __name__ == "__main__":
    run_acceptance()
