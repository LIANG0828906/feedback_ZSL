#!/usr/bin/env python3
"""End-to-end acceptance run against the configured local SQL Server."""

from __future__ import annotations

import argparse
import dataclasses
import http.client
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from orders_migration.config import Config
from orders_migration.errors import AppError
from orders_migration.service import MigrationService
from orders_migration.sqlclient import open_connection


def api(
    host: str,
    port: int,
    method: str,
    path: str,
    body: dict[str, Any] | None = None,
    headers: dict[str, str] | None = None,
) -> tuple[int, dict[str, Any]]:
    connection = http.client.HTTPConnection(host, port, timeout=60)
    payload = (
        json.dumps(body, separators=(",", ":")).encode("utf-8")
        if body is not None
        else None
    )
    all_headers = {"Content-Type": "application/json"}
    all_headers.update(headers or {})
    try:
        connection.request(method, path, body=payload, headers=all_headers)
        response = connection.getresponse()
        raw = response.read()
        return response.status, json.loads(raw.decode("utf-8")) if raw else {}
    finally:
        connection.close()


def wait_health(host: str, port: int, timeout: float = 20.0) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            status, body = api(host, port, "GET", "/healthz")
            if status == 200 and body.get("ready"):
                return body
        except Exception as exc:
            last_error = exc
        time.sleep(0.2)
    raise RuntimeError(f"service did not become healthy: {last_error}")


def start_server(
    config_path: Path, log_path: Path, environment: dict[str, str]
) -> subprocess.Popen[str]:
    handle = open(log_path, "a", encoding="utf-8")
    process = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "orders_migration",
            "--config",
            str(config_path),
            "serve",
        ],
        cwd=ROOT,
        stdout=handle,
        stderr=subprocess.STDOUT,
        text=True,
        env=environment,
    )
    handle.close()
    return process


def stop_server(process: subprocess.Popen[str] | None) -> None:
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def sql_execute(config: Config, database: str, sql: str,
                params: tuple[Any, ...] = ()) -> None:
    if database == "legacy":
        cs = config.database.legacy_connection_string
    else:
        index = int(database.removeprefix("shard"))
        cs = config.database.shard_connection_string(index)
    connection, dialect = open_connection(
        cs,
        config.database.driver,
        config.database.connect_timeout_seconds,
        config.database.command_timeout_seconds,
    )
    try:
        cursor = connection.cursor()
        cursor.execute(
            sql.replace("?", "%s") if dialect == "pymssql" else sql,
            params,
        )
        connection.commit()
        cursor.close()
    finally:
        connection.close()


def check_step(name: str, condition: bool, evidence: Any) -> dict[str, Any]:
    if not condition:
        raise AssertionError(f"{name} failed: {evidence}")
    return {"name": name, "passed": True, "evidence": evidence}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default=str(ROOT / "config.local.json"))
    parser.add_argument("--skip-bootstrap", action="store_true")
    parser.add_argument("--skip-load", action="store_true")
    parser.add_argument("--load-duration", type=float, default=20.0)
    parser.add_argument(
        "--report", default=str(ROOT / "artifacts" / "acceptance.json")
    )
    args = parser.parse_args()

    config_path = Path(args.config).resolve()
    config = Config.load(config_path)
    environment = dict(os.environ)
    report: dict[str, Any] = {
        "ok": False,
        "startedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "config": config_path.name,
        "steps": [],
    }
    steps: list[dict[str, Any]] = report["steps"]
    server: subprocess.Popen[str] | None = None
    log_path = ROOT / "artifacts" / "acceptance-service.log"
    log_path.parent.mkdir(parents=True, exist_ok=True)

    try:
        if not args.skip_bootstrap:
            for suffix in ("", "-wal", "-shm"):
                candidate = Path(str(config.sqlite_path) + suffix)
                if candidate.exists():
                    candidate.unlink()
            subprocess.run(
                [
                    sys.executable,
                    str(ROOT / "scripts" / "bootstrap_local.py"),
                    "--config",
                    str(config_path),
                    "--fresh",
                ],
                cwd=ROOT,
                check=True,
                env=environment,
            )
            steps.append(check_step("bootstrap_20k", True, {"orders": 20000}))

        server = start_server(config_path, log_path, environment)
        health = wait_health(config.host, config.port)
        steps.append(check_step("topology_startup", True, health["schemaSignature"]))

        status, before = api(
            config.host,
            config.port,
            "GET",
            "/api/v1/orders/2?tenantId=2",
        )
        steps.append(
            check_step(
                "initial_route_legacy",
                status == 200 and before["route"]["mode"] == "legacy",
                before["route"],
            )
        )

        status, written = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/orders",
            {
                "orderId": 2,
                "tenantId": 2,
                "status": 4,
                "amount": "321.09",
                "currency": "USD",
                "updatedAt": "2026-11-19T02:00:00Z",
            },
        )
        steps.append(
            check_step(
                "dual_write_success",
                status == 200
                and written.get("ok")
                and bool(written.get("legacyCommitSite"))
                and bool(written.get("shardCommitSite")),
                {
                    "writeId": written.get("writeId"),
                    "legacyCommitSite": written.get("legacyCommitSite"),
                    "shardCommitSite": written.get("shardCommitSite"),
                },
            )
        )

        status, _ = api(
            config.host,
            config.port,
            "POST",
            "/admin/faults",
            {"stage": "after_shard_commit", "times": 1},
            {"X-Admin-Token": config.admin_token or ""},
        )
        if status != 200:
            raise RuntimeError(f"cannot inject dual-write fault: {status}")
        status, failed_write = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/orders",
            {
                "orderId": 2,
                "tenantId": 2,
                "status": 1,
                "amount": "999.99",
                "currency": "EUR",
                "updatedAt": "2026-11-19T02:01:00Z",
            },
        )
        _, readback = api(
            config.host,
            config.port,
            "GET",
            "/api/v1/orders/2?tenantId=2",
        )
        steps.append(
            check_step(
                "dual_write_compensates",
                status == 409
                and failed_write.get("errorCode") == "E_DUAL_WRITE"
                and readback["order"]["amount"] == "321.09",
                {
                    "errorCode": failed_write.get("errorCode"),
                    "amountAfterFailure": readback["order"]["amount"],
                },
            )
        )

        status, _ = api(
            config.host,
            config.port,
            "POST",
            "/admin/faults",
            {"stage": "after_shard_batch_commit", "times": 1},
            {"X-Admin-Token": config.admin_token or ""},
        )
        if status != 200:
            raise RuntimeError(f"cannot inject migration fault: {status}")
        status, interrupted = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/migrations",
            {"tenantId": 2, "maxBatches": 1},
        )
        steps.append(
            check_step(
                "migration_interrupts_after_commit",
                status == 409
                and interrupted.get("errorCode") == "E_MIGRATION_INTERRUPTED",
                interrupted.get("details"),
            )
        )

        stop_server(server)
        server = start_server(config_path, log_path, environment)
        wait_health(config.host, config.port)
        status, resumed = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/migrations",
            {"tenantId": 2, "maxBatches": 1},
        )
        steps.append(
            check_step(
                "migration_resumes_after_restart",
                status == 200
                and resumed["status"] == "IN_PROGRESS"
                and resumed["lastOrderId"] > 0,
                resumed,
            )
        )

        shard_config = dataclasses.replace(
            config,
            migration=dataclasses.replace(
                config.migration, read_mode_during_migration="shard"
            ),
        )
        shard_view = MigrationService.create(shard_config)
        try:
            try:
                shard_view.get_order(19986, 2)
                not_migrated: dict[str, Any] = {}
            except AppError as exc:
                not_migrated = {"code": exc.code, "details": exc.details}
        finally:
            shard_view.close()
        _, shard_route = api(
            config.host,
            config.port,
            "GET",
            "/api/v1/orders/2?tenantId=2",
        )
        steps.append(
            check_step(
                "during_migration_shard_route_never_regresses",
                not_migrated.get("code") == "E_NOT_MIGRATED"
                and not_migrated.get("details", {}).get("orderId") == 19986
                and shard_route["route"]["mode"] == "shard",
                {
                    "unmigratedRead": not_migrated,
                    "migratedReadRoute": shard_route["route"],
                },
            )
        )

        status, completed = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/migrations",
            {"tenantId": 2},
        )
        _, after_route = api(
            config.host,
            config.port,
            "GET",
            "/api/v1/orders/2?tenantId=2",
        )
        steps.append(
            check_step(
                "migration_completes_and_routes_shard",
                status == 200
                and completed["status"] == "COMPLETE"
                and after_route["route"]["mode"] == "shard",
                {
                    "migration": completed,
                    "route": after_route["route"],
                },
            )
        )

        status, clean_check = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/checks",
            {"tenantId": 2, "force": True},
        )
        steps.append(
            check_step(
                "clean_reconciliation",
                status == 200
                and clean_check["status"] == "COMPLETE"
                and clean_check["mismatchCount"] == 0,
                {
                    "runId": clean_check.get("runId"),
                    "totalChecked": clean_check.get("totalChecked"),
                },
            )
        )

        sql_execute(
            config,
            "shard2",
            """
            UPDATE dbo.Orders
            SET Amount = Amount + 7.77,
                UpdatedAt = DATEADD(SECOND, -2, UpdatedAt)
            WHERE OrderId = ?
            """,
            (2,),
        )
        status, drift_check = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/checks",
            {"tenantId": 2, "force": True},
        )
        amount_mismatch = next(
            (
                row
                for row in drift_check.get("mismatches", [])
                if row["orderId"] == 2 and row["fieldName"] == "amount"
            ),
            None,
        )
        steps.append(
            check_step(
                "reconciliation_reports_field_values",
                status == 200
                and amount_mismatch is not None
                and amount_mismatch["legacyValue"] != amount_mismatch["shardValue"],
                amount_mismatch,
            )
        )

        status, rollback = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/rollbacks",
            {"tenants": [2]},
        )
        _, rolled_back = api(
            config.host,
            config.port,
            "GET",
            "/api/v1/orders/2?tenantId=2",
        )
        steps.append(
            check_step(
                "controlled_rollback_converges",
                status == 200
                and rollback["status"] == "COMPLETE"
                and rollback["completedTenants"] == [2]
                and rolled_back["route"]["mode"] == "legacy"
                and rolled_back["order"]["amount"] == "321.09",
                {
                    "rollback": rollback,
                    "route": rolled_back["route"],
                    "amount": rolled_back["order"]["amount"],
                },
            )
        )

        status, final_check = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/checks",
            {"tenantId": 2, "force": True},
        )
        steps.append(
            check_step(
                "post_rollback_field_equality",
                status == 200 and final_check["mismatchCount"] == 0,
                {
                    "runId": final_check.get("runId"),
                    "totalChecked": final_check.get("totalChecked"),
                },
            )
        )

        status, _ = api(
            config.host,
            config.port,
            "POST",
            "/admin/faults",
            {"stage": "during_check", "times": 1},
            {"X-Admin-Token": config.admin_token or ""},
        )
        if status != 200:
            raise RuntimeError(f"cannot inject check fault: {status}")
        status, interrupted_check = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/checks",
            {"tenantId": 3, "force": True},
        )
        check_run_id = interrupted_check.get("details", {}).get("runId")
        status_resume, resumed_check = api(
            config.host,
            config.port,
            "POST",
            f"/api/v1/checks/{check_run_id}/resume",
        )
        steps.append(
            check_step(
                "check_failure_preserves_partial_and_resumes",
                status == 503
                and interrupted_check.get("errorCode") == "E_CHECK_UNAVAILABLE"
                and bool(check_run_id)
                and status_resume == 200
                and resumed_check.get("status") == "COMPLETE"
                and resumed_check.get("cursorOrderId", 0) > 0,
                {
                    "errorCode": interrupted_check.get("errorCode"),
                    "runId": check_run_id,
                    "resumedStatus": resumed_check.get("status"),
                    "cursorOrderId": resumed_check.get("cursorOrderId"),
                    "mismatchCount": resumed_check.get("mismatchCount"),
                },
            )
        )

        status, _ = api(
            config.host,
            config.port,
            "POST",
            "/admin/faults",
            {"stage": "after_rollback_batch", "times": 1},
            {"X-Admin-Token": config.admin_token or ""},
        )
        if status != 200:
            raise RuntimeError(f"cannot inject rollback fault: {status}")
        status, interrupted_rollback = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/rollbacks",
            {"tenants": [3, 4]},
        )
        rollback_run_id = interrupted_rollback.get("details", {}).get("runId")
        status_resume, resumed_rollback = api(
            config.host,
            config.port,
            "POST",
            f"/api/v1/rollbacks/{rollback_run_id}/resume",
        )
        steps.append(
            check_step(
                "rollback_failure_reports_tenants_and_resumes",
                status == 409
                and interrupted_rollback.get("errorCode") == "E_ROLLBACK"
                and interrupted_rollback.get("details", {})
                .get("writeGate", {})
                .get("stopping")
                == 1
                and bool(rollback_run_id)
                and status_resume == 200
                and resumed_rollback.get("status") == "COMPLETE"
                and resumed_rollback.get("pendingTenants") == [],
                {
                    "errorCode": interrupted_rollback.get("errorCode"),
                    "runId": rollback_run_id,
                    "completedOnFailure": interrupted_rollback.get("details", {}).get(
                        "completedTenants"
                    ),
                    "pendingOnFailure": interrupted_rollback.get("details", {}).get(
                        "pendingTenants"
                    ),
                    "resumed": resumed_rollback.get("status"),
                },
            )
        )

        status, trace = api(
            config.host, config.port, "GET", "/api/v1/trace/2"
        )
        steps.append(
            check_step(
                "order_trace_complete",
                status == 200 and len(trace["events"]) >= 5,
                {
                    "eventCount": len(trace["events"]),
                    "dualWrites": len(trace["dualWrites"]),
                    "rollbackActions": len(trace["rollbackActions"]),
                },
            )
        )

        bad_config = json.loads(config_path.read_text(encoding="utf-8"))
        bad_config["database"]["shard_connection_string_template"] = (
            bad_config["database"]["shard_connection_string_template"].replace(
                "OrdersMigrationShard{shard}", "OrdersMigrationMissing{shard}"
            )
        )
        bad_path = ROOT / "runtime" / "bad-topology.json"
        bad_path.parent.mkdir(parents=True, exist_ok=True)
        bad_path.write_text(json.dumps(bad_config, indent=2), encoding="utf-8")
        topology_result = subprocess.run(
            [
                sys.executable,
                "-m",
                "orders_migration",
                "--config",
                str(bad_path),
                "validate",
            ],
            cwd=ROOT,
            text=True,
            capture_output=True,
            env=environment,
        )
        try:
            topology_body = json.loads(topology_result.stdout)
        except json.JSONDecodeError:
            topology_body = {"stdout": topology_result.stdout}
        steps.append(
            check_step(
                "topology_failure_rejects_startup",
                topology_result.returncode == 3
                and topology_body.get("errorCode") == "E_TOPOLOGY",
                {
                    "exitCode": topology_result.returncode,
                    "errorCode": topology_body.get("errorCode"),
                },
            )
        )

        if not args.skip_load:
            load_report = ROOT / "artifacts" / "acceptance-load.json"
            load_result = subprocess.run(
                [
                    sys.executable,
                    str(ROOT / "scripts" / "loadtest.py"),
                    "--duration",
                    str(args.load_duration),
                    "--writes-rate",
                    "20",
                    "--reads-rate",
                    "30",
                    "--tenant-id",
                    "2",
                    "--report-out",
                    str(load_report),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                env=environment,
            )
            load_body = json.loads(load_report.read_text(encoding="utf-8"))
            steps.append(
                check_step(
                    "mixed_load_slo",
                    load_result.returncode == 0 and load_body["ok"],
                    load_body["observed"],
                )
            )

        report["ok"] = all(step["passed"] for step in steps)
        report["completedAt"] = time.strftime(
            "%Y-%m-%dT%H:%M:%SZ", time.gmtime()
        )
        return 0 if report["ok"] else 1
    except Exception as exc:
        report["error"] = {
            "type": type(exc).__name__,
            "message": str(exc),
        }
        report["completedAt"] = time.strftime(
            "%Y-%m-%dT%H:%M:%SZ", time.gmtime()
        )
        return 1
    finally:
        stop_server(server)
        output = Path(args.report)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(
            json.dumps(report, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        print(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    raise SystemExit(main())
