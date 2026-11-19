from __future__ import annotations

import argparse
import json
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .errors import ExecutionStopped, SettlementError
from .generator import GeneratorConfig, generate_dataset
from .util import canonical_json, pretty_json


def _add_data_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--out", required=True, help="dataset output directory")
    parser.add_argument("--seed", type=int, default=20260101)
    parser.add_argument("--start", default="2026-01-01T00:00:00Z")
    parser.add_argument("--days", type=int, default=3)
    parser.add_argument("--telemetry-rows", type=int, default=80_000)
    parser.add_argument("--supplement-rows", type=int, default=30_000)
    parser.add_argument("--batches", type=int, default=96)
    parser.add_argument("--vehicles", type=int, default=200)
    parser.add_argument("--duplicate-ratio", type=float, default=0.05)
    parser.add_argument("--late-ratio", type=float, default=0.03)
    parser.add_argument("--overwrite", action="store_true")


def _add_execution_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--sql", required=True, help="settlement SQL file")
    parser.add_argument("--data", required=True, help="generated dataset directory")
    parser.add_argument("--db", default="settlement.db", help="local SQLite settlement database")
    parser.add_argument(
        "--mode",
        choices=["pushdown", "replay", "both"],
        default="both",
        help="pushdown scan, no-pushdown replay, or both",
    )
    parser.add_argument("--slots", type=int, default=5, help="logical processing slots (minimum 3)")
    parser.add_argument("--cpu-workers", type=int, default=2)
    parser.add_argument("--allowed-lateness-seconds", type=int, default=180)
    parser.add_argument("--skew-threshold", type=float, default=0.08)
    parser.add_argument("--memory-limit-mb", type=int, default=1024)
    parser.add_argument("--spill-threshold-mb", type=int, default=64)
    parser.add_argument("--temp-limit-mb", type=int, default=2048)
    parser.add_argument("--temp-dir", default=".settlement_tmp")
    parser.add_argument(
        "--stop-after-batch",
        type=int,
        default=None,
        help="commit this batch, then exit with code 75 (external termination simulation)",
    )
    parser.add_argument(
        "--fail-batch",
        type=int,
        default=None,
        help="inject one pre-commit failure for this batch; the next attempt succeeds",
    )
    parser.add_argument("--reset", action="store_true", help="discard the matching run state first")
    parser.add_argument("--report-out", default="job_report.json")
    parser.add_argument("--quiet", action="store_true")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="telemetry-settlement",
        description="Resumable local SQL settlement executor for telemetry and charging supplements",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate the fixed-seed local dataset")
    _add_data_options(generate)

    plan = sub.add_parser("plan", help="parse SQL and print the safe pushdown plan")
    plan.add_argument("--sql", required=True)
    plan.add_argument("--data", required=True)
    plan.add_argument("--out", default=None)

    for name in ("run", "verify"):
        command = sub.add_parser(
            name,
            help="execute settlement SQL"
            + (" and compare pushdown/replay" if name == "verify" else ""),
        )
        _add_execution_options(command)
        if name == "verify":
            command.set_defaults(mode="both")

    report = sub.add_parser("report", help="print a stored job report")
    report.add_argument("--db", default="settlement.db")
    report.add_argument("--mode", choices=["pushdown", "replay"], default=None)
    report.add_argument("--out", default=None)

    inspect = sub.add_parser("inspect", help="inspect published vehicle/window rows")
    inspect.add_argument("--db", default="settlement.db")
    inspect.add_argument("--mode", choices=["pushdown", "replay"], default="pushdown")
    inspect.add_argument("--vehicle", default=None)
    inspect.add_argument("--limit", type=int, default=20)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            return _cmd_generate(args)
        if args.command == "plan":
            return _cmd_plan(args)
        if args.command in {"run", "verify"}:
            return _cmd_run(args)
        if args.command == "report":
            return _cmd_report(args)
        if args.command == "inspect":
            return _cmd_inspect(args)
        parser.error("unknown command")
    except SettlementError as exc:
        print(pretty_json(exc.as_dict()).strip(), file=sys.stderr)
        return _error_exit_code(exc.code)
    except KeyboardInterrupt:
        print(
            pretty_json(
                {
                    "code": "E_STOPPED",
                    "message": "interrupted; committed checkpoints remain resumable",
                }
            ).strip(),
            file=sys.stderr,
        )
        return 75
    except Exception as exc:
        print(
            pretty_json({"code": "E_INTERNAL", "message": str(exc)}).strip(),
            file=sys.stderr,
        )
        return 1
    return 0


def _cmd_generate(args: argparse.Namespace) -> int:
    cfg = GeneratorConfig(
        seed=args.seed,
        start=_parse_cli_datetime(args.start),
        days=args.days,
        telemetry_rows=args.telemetry_rows,
        supplement_rows=args.supplement_rows,
        batches=args.batches,
        duplicate_ratio=args.duplicate_ratio,
        late_ratio=args.late_ratio,
        vehicle_count=args.vehicles,
    )
    manifest = generate_dataset(args.out, cfg, overwrite=args.overwrite)
    print(
        canonical_json(
            {
                "status": "generated",
                "manifest": str(manifest),
                "telemetry_rows": cfg.telemetry_rows,
                "supplement_rows": cfg.supplement_rows,
                "batches": cfg.batches,
            }
        )
    )
    return 0


def _cmd_plan(args: argparse.Namespace) -> int:
    from .engine import ExecutionConfig, Executor

    plan = Executor(
        ExecutionConfig(sql=args.sql, data_dir=args.data, db_path=":memory:")
    ).plan()
    text = pretty_json(plan)
    if args.out:
        path = Path(args.out)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
        print(canonical_json({"status": "planned", "plan": str(path)}))
    else:
        print(text, end="")
    return 0


def _cmd_run(args: argparse.Namespace) -> int:
    from .engine import ExecutionConfig, Executor

    config = ExecutionConfig(
        sql=args.sql,
        data_dir=args.data,
        db_path=args.db,
        mode="both" if args.command == "verify" else args.mode,
        slots=args.slots,
        cpu_workers=args.cpu_workers,
        allowed_lateness_seconds=args.allowed_lateness_seconds,
        skew_threshold_ratio=args.skew_threshold,
        memory_limit_bytes=args.memory_limit_mb * 1024 * 1024,
        spill_threshold_bytes=args.spill_threshold_mb * 1024 * 1024,
        temp_limit_bytes=args.temp_limit_mb * 1024 * 1024,
        temp_dir=args.temp_dir,
        stop_after_batch=args.stop_after_batch,
        fail_batch=args.fail_batch,
        reset=args.reset,
    )
    try:
        results = Executor(config).run()
    except ExecutionStopped as exc:
        payload = _stopped_output(args, exc)
        report_path = Path(args.report_out)
        report_path.parent.mkdir(parents=True, exist_ok=True)
        report_path.write_text(pretty_json(payload), encoding="utf-8")
        if not args.quiet:
            print(canonical_json(payload))
        return 75
    payload = _combined_output(results)
    report_path = Path(args.report_out)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(pretty_json(_full_report_output(results)), encoding="utf-8")
    if not args.quiet:
        print(canonical_json(payload))
    return 0


def _stopped_output(args: argparse.Namespace, exc: ExecutionStopped) -> dict[str, Any]:
    run_id = exc.details.get("run_id")
    report: dict[str, Any] | None = None
    if run_id:
        connection = sqlite3.connect(str(args.db))
        connection.row_factory = sqlite3.Row
        try:
            row = connection.execute(
                "SELECT report_json FROM job_reports WHERE run_id=?", (run_id,)
            ).fetchone()
            if row:
                report = json.loads(row["report_json"])
        finally:
            connection.close()
    return {
        "status": "stopped",
        "run_id": run_id,
        "committed_batch": exc.details.get("committed_batch"),
        "commit_seq": exc.details.get("commit_seq"),
        "resumable": True,
        "report": report,
        "message": exc.message,
    }


def _combined_output(results: list[Any]) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "status": results[-1].status if results else "unknown",
        "runs": [
            {
                "run_id": result.run_id,
                "plan_mode": result.plan_mode,
                "status": result.status,
                "verified": result.verified,
                "stable_hash": result.report.get("stable_hash"),
                "late_events": result.report.get("late", {}).get("count"),
                "dedup_discards": result.report.get("dedup", {}).get("discarded"),
                "max_slot_share": result.report.get("slots", {}).get("max_share"),
                "resume_count": result.report.get("checkpoints", {}).get("resume_count"),
                "spill_bytes": result.report.get("spill", {}).get("bytes"),
            }
            for result in results
        ],
    }
    if len(results) == 2:
        payload["verification"] = results[-1].report.get("verification")
    return payload


def _full_report_output(results: list[Any]) -> dict[str, Any]:
    if len(results) == 1:
        return results[0].report
    by_mode = {result.plan_mode: result.report for result in results}
    return {
        "report_version": 1,
        "plan_mode": "both",
        "inputs": by_mode["pushdown"].get("inputs") if "pushdown" in by_mode else None,
        "predicates": (
            by_mode["pushdown"].get("predicates") if "pushdown" in by_mode else None
        ),
        "pushdown": by_mode.get("pushdown"),
        "replay": by_mode.get("replay"),
        "verification": results[-1].report.get("verification"),
    }


def _cmd_report(args: argparse.Namespace) -> int:
    path = Path(args.db)
    if not path.exists():
        raise SettlementError("E_STATE", f"settlement database not found: {path}")
    connection = sqlite3.connect(str(path))
    connection.row_factory = sqlite3.Row
    try:
        if args.mode:
            row = connection.execute(
                "SELECT report_json FROM job_reports WHERE plan_mode=? "
                "ORDER BY created_at DESC LIMIT 1",
                (args.mode,),
            ).fetchone()
            report = json.loads(row["report_json"]) if row else None
        else:
            rows = connection.execute(
                "SELECT plan_mode,report_json FROM job_reports ORDER BY created_at DESC"
            ).fetchall()
            latest: dict[str, dict[str, Any]] = {}
            for candidate in rows:
                latest.setdefault(candidate["plan_mode"], json.loads(candidate["report_json"]))
            if len(latest) >= 2 and "pushdown" in latest and "replay" in latest:
                report = {
                    "report_version": 1,
                    "plan_mode": "both",
                    "inputs": latest["pushdown"].get("inputs"),
                    "predicates": latest["pushdown"].get("predicates"),
                    "pushdown": latest["pushdown"],
                    "replay": latest["replay"],
                    "verification": latest["replay"].get("verification"),
                }
            elif latest:
                report = next(iter(latest.values()))
            else:
                report = None
        if report is None:
            raise SettlementError("E_STATE", "no stored job report found")
    finally:
        connection.close()
    text = pretty_json(report)
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
        print(canonical_json({"status": "reported", "report": args.out}))
    else:
        print(text, end="")
    return 0


def _cmd_inspect(args: argparse.Namespace) -> int:
    path = Path(args.db)
    if not path.exists():
        raise SettlementError("E_STATE", f"settlement database not found: {path}")
    connection = sqlite3.connect(str(path))
    connection.row_factory = sqlite3.Row
    sql = (
        "SELECT vehicle_id,window_start,mileage_delta,energy_delta,energy_per_100km,"
        "event_count,telemetry_count,supplement_count FROM vehicle_energy "
        "WHERE plan_mode=?"
    )
    parameters: list[Any] = [args.mode]
    if args.vehicle:
        sql += " AND vehicle_id=?"
        parameters.append(args.vehicle)
    sql += " ORDER BY vehicle_id,window_start LIMIT ?"
    parameters.append(max(0, args.limit))
    try:
        rows = [dict(row) for row in connection.execute(sql, parameters)]
    finally:
        connection.close()
    print(pretty_json({"mode": args.mode, "rows": rows}), end="")
    return 0


def _parse_cli_datetime(value: str) -> datetime:
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed


def _error_exit_code(code: str) -> int:
    return {
        "E_STOPPED": 75,
        "E_MEMORY": 75,
        "E_SQL": 2,
        "E_PLAN": 2,
        "E_DATA": 3,
        "E_STATE": 4,
    }.get(code, 1)


if __name__ == "__main__":
    raise SystemExit(main())

