from __future__ import annotations

import argparse
import json
import multiprocessing as mp
import sys
from pathlib import Path
from typing import Any

from .config import RunConfig
from .errors import EngineError
from .generator import GeneratorConfig, generate
from .orchestrator import run_job, state_snapshot, status
from .sql_parser import parse_sql
from .verify import query_vehicle, verify_jobs


def _json_print(value: Any, stream=sys.stdout) -> None:
    json.dump(value, stream, ensure_ascii=False, sort_keys=True, indent=2, default=str)
    stream.write("\n")


def main(argv: list[str] | None = None) -> int:
    mp.freeze_support()
    parser = argparse.ArgumentParser(prog="iovflow", description="Event-time DAG engine")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("gen", help="generate the fixed-seed 96-batch workload")
    p.add_argument("--out", default="data")
    p.add_argument("--seed", type=int, default=3202026)
    p.add_argument("--rows", type=int, default=80000)
    p.add_argument("--batches", type=int, default=96)
    p.add_argument("--vehicles", type=int, default=1200)
    p.add_argument("--trips-per-vehicle", type=int, default=4)
    p.add_argument("--duplicate-ratio", type=float, default=0.04)
    p.add_argument("--delayed-ratio", type=float, default=0.03)
    p.add_argument("--hot-vehicle-share", type=float, default=0.12)

    p = sub.add_parser("plan", help="parse SQL and print the operator DAG")
    p.add_argument("--sql", default="sql/window_5m.sql")
    p.add_argument("--vehicle-column", default="vehicle_id")
    p.add_argument("--parallelism", type=int, default=2)
    p.add_argument("--max-shards", type=int, default=4)

    p = sub.add_parser("run", help="run a batch or stream job")
    p.add_argument("--sql", default="sql/window_5m.sql")
    p.add_argument("--input", default="data")
    p.add_argument("--mode", choices=("batch", "stream"), default="stream")
    p.add_argument("--job-id", default=None)
    p.add_argument("--state-db", default="state/iovflow_state.db")
    p.add_argument("--results-db", default="results/results.db")
    p.add_argument("--temp-dir", default="tmp")
    p.add_argument("--report-dir", default="reports")
    p.add_argument("--parallelism", type=int, default=2)
    p.add_argument("--max-shards", type=int, default=4)
    p.add_argument("--allowed-lateness", type=int, default=300)
    p.add_argument("--state-memory-mb", type=float, default=32.0)
    p.add_argument("--memory-limit-mb", type=int, default=1024)
    p.add_argument("--temp-limit-mb", type=int, default=2048)
    p.add_argument("--no-progress-timeout", type=float, default=30.0)
    p.add_argument("--max-takeovers", type=int, default=8)
    p.add_argument("--vehicle-column", default="vehicle_id")
    p.add_argument("--dedupe-key", default="vehicle_id,trip_id,event_time,event_type")
    p.add_argument("--shard-skew-cap", type=float, default=0.40)
    p.add_argument("--keep-temp", action="store_true")
    p.add_argument("--resume", action="store_true")
    p.add_argument("--inject-kill", default=None, help="stage=...,batch=...,shard=...")
    p.add_argument("--inject-hang", default=None, help="stage=...,batch=...,shard=...")
    p.add_argument("--inject-delay-ms", type=int, default=40)
    p.add_argument("--metrics-port", type=int, default=None)

    p = sub.add_parser("status", help="query job, stage, takeover, and window status")
    p.add_argument("--job-id", required=True)
    p.add_argument("--state-db", default="state/iovflow_state.db")
    p.add_argument("--results-db", default="results/results.db")

    p = sub.add_parser("state", help="dump deterministic state after a batch")
    p.add_argument("--job-id", required=True)
    p.add_argument("--batch", type=int, default=None)
    p.add_argument("--state-db", default="state/iovflow_state.db")

    p = sub.add_parser("query", help="query final windows for a vehicle")
    p.add_argument("--job-id", required=True)
    p.add_argument("--vehicle", required=True)
    p.add_argument("--results-db", default="results/results.db")
    p.add_argument("--all-versions", action="store_true")

    p = sub.add_parser("verify", help="compare two jobs window-by-window")
    p.add_argument("--job-a", required=True)
    p.add_argument("--job-b", required=True)
    p.add_argument("--results-db", default="results/results.db")
    p.add_argument("--state-db", default=None)
    p.add_argument("--tolerance", type=float, default=1e-6)

    args = parser.parse_args(argv)
    try:
        if args.command == "gen":
            manifest = generate(
                GeneratorConfig(
                    out_dir=args.out,
                    seed=args.seed,
                    rows=args.rows,
                    batches=args.batches,
                    vehicles=args.vehicles,
                    trips_per_vehicle=args.trips_per_vehicle,
                    duplicate_ratio=args.duplicate_ratio,
                    delayed_ratio=args.delayed_ratio,
                    hot_vehicle_share=args.hot_vehicle_share,
                )
            )
            _json_print(manifest["stats"])
            return 0
        if args.command == "plan":
            plan = parse_sql(Path(args.sql).read_text(encoding="utf-8"), args.vehicle_column)
            out = plan.to_dict()
            out["stages"] = plan.default_stages(args.parallelism, args.max_shards)
            for stage in out["stages"]:
                if stage["stage_type"] == "window_aggregate":
                    stage["parallelism"] = max(args.parallelism, args.max_shards)
            _json_print(out)
            return 0
        if args.command == "run":
            job_id = args.job_id or f"{args.mode}-{int(__import__('time').time())}-{__import__('os').getpid()}"
            if args.resume and not args.job_id:
                raise EngineError(
                    "IOV_STATE_INCOMPATIBLE", "--resume requires an explicit --job-id"
                )
            inject_spec = args.inject_kill
            inject_delay = args.inject_delay_ms
            if args.inject_hang:
                inject_spec = (args.inject_hang + ",mode=hang").strip(",")
                # A suspended worker must be stopped immediately so it cannot
                # finish before the no-progress watchdog fires.
                inject_delay = 0
            config = RunConfig(
                job_id=job_id,
                sql_path=args.sql,
                input_dir=args.input,
                mode=args.mode,
                state_db=args.state_db,
                results_db=args.results_db,
                temp_dir=args.temp_dir,
                report_dir=args.report_dir,
                parallelism=args.parallelism,
                max_shards=args.max_shards,
                allowed_lateness=args.allowed_lateness,
                state_memory_mb=args.state_memory_mb,
                memory_limit_mb=args.memory_limit_mb,
                temp_limit_mb=args.temp_limit_mb,
                no_progress_timeout=args.no_progress_timeout,
                max_takeovers=args.max_takeovers,
                vehicle_column=args.vehicle_column,
                dedupe_keys=tuple(x.strip() for x in args.dedupe_key.split(",") if x.strip()),
                shard_skew_cap=args.shard_skew_cap,
                keep_temp=args.keep_temp,
                resume=args.resume,
                inject_kill=inject_spec,
                inject_delay_ms=inject_delay,
                metrics_port=args.metrics_port,
            )
            report = run_job(config)
            _json_print(report)
            return 0
        if args.command == "status":
            _json_print(status(args.job_id, args.state_db, args.results_db))
            return 0
        if args.command == "state":
            _json_print(state_snapshot(args.job_id, args.state_db, args.batch))
            return 0
        if args.command == "query":
            _json_print(
                query_vehicle(
                    args.job_id,
                    args.vehicle,
                    args.results_db,
                    final_only=not args.all_versions,
                )
            )
            return 0
        if args.command == "verify":
            result = verify_jobs(
                args.job_a,
                args.job_b,
                args.results_db,
                args.state_db,
                args.tolerance,
            )
            _json_print(result)
            return 0 if result["ok"] else 1
        raise EngineError("IOV_INTERNAL", f"unknown command {args.command}")
    except EngineError as exc:
        _json_print(exc.to_dict(), sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

