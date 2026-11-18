#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "src"))

from orderlab_gateway.cli import command_index, command_init_db  # noqa: E402
from orderlab_gateway.config import load_config  # noqa: E402
from orderlab_gateway.db import connect  # noqa: E402
from orderlab_gateway.engine import QueryEngine  # noqa: E402
from orderlab_gateway.replay import ReplayRunner, compare_reports, write_report  # noqa: E402


INDEX_NAME = "IX_OrderHeader_Tenant_Status_OrderDate"


def _record_map(report: dict[str, Any]) -> dict[tuple[str, str], dict[str, Any]]:
    return {
        (str(record["query_id"]), str(record["dialect"])): record
        for record in report["records"]
    }


def _same_record(left: dict[str, Any], right: dict[str, Any]) -> bool:
    return (
        left.get("outcome") == right.get("outcome")
        and left.get("error_code") == right.get("error_code")
        and left.get("columns") == right.get("columns")
        and left.get("rows") == right.get("rows")
        and left.get("result_digest") == right.get("result_digest")
    )


def _header_count(config) -> int:
    connection = connect(config.database)
    try:
        cursor = connection.cursor()
        cursor.execute("SELECT COUNT_BIG(*) FROM dbo.OrderHeader")
        return int(cursor.fetchone()[0])
    finally:
        connection.close()


def evaluate(config_path: str, *, initialize: bool) -> dict[str, Any]:
    config = load_config(config_path)
    if initialize:
        command_init_db(config)
    before_count = _header_count(config)
    engine = QueryEngine(config)
    runner = ReplayRunner(config, engine)

    baseline = runner.run(concurrency=2)
    write_report(baseline, ROOT / "artifacts/replay_baseline.json")

    disabled: dict[str, Any]
    try:
        command_index(config, INDEX_NAME, "disabled")
        disabled = runner.run(concurrency=2)
        write_report(disabled, ROOT / "artifacts/replay_index_disabled.json")
    finally:
        command_index(config, INDEX_NAME, "enabled")

    restored_engine = QueryEngine(config)
    restored = ReplayRunner(config, restored_engine).run(concurrency=2)
    write_report(restored, ROOT / "artifacts/replay_restored.json")
    comparison = compare_reports(baseline, disabled, restored)
    write_report(comparison, ROOT / "artifacts/replay_comparison.json")

    baseline_map = _record_map(baseline)
    disabled_map = _record_map(disabled)
    restored_map = _record_map(restored)

    dialect_mismatches = []
    query_ids = sorted({query_id for query_id, _dialect in baseline_map})
    for query_id in query_ids:
        current = baseline_map.get((query_id, "mssql-current"))
        legacy = baseline_map.get((query_id, "mssql-legacy"))
        if current is None or legacy is None or not _same_record(current, legacy):
            dialect_mismatches.append(query_id)

    illegal_ok = all(
        record["outcome"] == "rejected" and record["error_code"] == "E_READONLY"
        for record in baseline["records"]
        if record["category"] == "illegal_write"
    )
    uncovered_ok = all(
        record["outcome"] == "rejected" and record["error_code"] == "E_INDEX_UNCOVERED"
        for record in baseline["records"]
        if record["category"] == "uncovered"
    )
    index_records = [
        record for record in baseline["records"] if record["category"] == "index_hit"
    ]
    plan_ok = all(
        record["outcome"] == "succeeded"
        and record["access"] == "index_seek"
        and record["index_name"]
        and all(
            "scan" not in str(row.get("physical_op", "")).lower()
            for row in record["plan_rows"]
        )
        for record in index_records
    )
    summary_records = [
        record for record in baseline["records"] if record["category"] == "degrade"
    ]
    summary_ok = all(
        record["outcome"] == "degraded"
        and bool(record["summary_row_keys"])
        and all(record["summary_row_keys"])
        for record in summary_records
    )
    disabled_dependents = [
        key
        for key, record in baseline_map.items()
        if record["outcome"] == "succeeded" and INDEX_NAME in record["index_name"].split(";")
    ]
    disabled_rejection_ok = all(
        disabled_map[key]["error_code"] == "E_INDEX_UNAVAILABLE"
        for key in disabled_dependents
        if key in disabled_map
    )
    restored_ok = all(
        _same_record(record, restored_map[key])
        for key, record in baseline_map.items()
        if key in restored_map
    )
    after_count = _header_count(config)
    no_write_side_effect = before_count == after_count == 20000
    checks = {
        "two_dialects_row_identical": not dialect_mismatches,
        "illegal_writes_rejected": illegal_ok,
        "uncovered_queries_rejected": uncovered_ok,
        "covering_index_seek_plans": plan_ok,
        "summary_fallback_traceable": summary_ok,
        "disabled_index_rejected_dependents": disabled_rejection_ok,
        "restored_results_match_baseline": restored_ok,
        "comparison_all_rows_consistent": comparison["all_rows_consistent_after_restore"],
        "rejected_writes_had_no_side_effect": no_write_side_effect,
    }
    report = {
        "schema_version": 1,
        "checks": checks,
        "passed": all(checks.values()),
        "dialect_mismatches": dialect_mismatches,
        "disabled_dependents": [f"{query_id}|{dialect}" for query_id, dialect in disabled_dependents],
        "header_count_before": before_count,
        "header_count_after": after_count,
        "artifact_files": [
            "artifacts/replay_baseline.json",
            "artifacts/replay_index_disabled.json",
            "artifacts/replay_restored.json",
            "artifacts/replay_comparison.json",
        ],
        "record_counts": {
            "baseline": len(baseline["records"]),
            "disabled": len(disabled["records"]),
            "restored": len(restored["records"]),
        },
    }
    write_report(report, ROOT / "artifacts/evaluation_report.json")
    return report


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default="config/gateway.json")
    parser.add_argument("--skip-init", action="store_true")
    args = parser.parse_args()
    result = evaluate(args.config, initialize=not args.skip_init)
    print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
