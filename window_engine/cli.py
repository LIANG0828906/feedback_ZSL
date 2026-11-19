"""Command line entry points: data generation and job execution."""

from __future__ import annotations

import argparse
import json
import os
import sys
import time

from .batch import run_batch
from .config import FaultPlan, JobConfig
from .datagen import GenSpec, generate
from .engine import Engine, KillAfterCommit
from .errors import EngineError
from .io_sources import key_counts_duckdb
from .report import write_report
from .semantics import SplitPlan
from .util import read_json


def _load_manifest(cfg: JobConfig) -> dict:
    return read_json(os.path.join(cfg.data_dir, cfg.manifest))


def _build_split_plan(cfg: JobConfig, manifest: dict) -> SplitPlan:
    counts = key_counts_duckdb(cfg.data_dir, manifest)
    return SplitPlan.build(counts, cfg.partitions, cfg.skew_ratio)


def cmd_generate(args: argparse.Namespace) -> int:
    spec = GenSpec(
        seed=args.seed,
        history_rows=args.history_rows,
        incremental_rows=args.incremental_rows,
        batches=args.batches,
    )
    manifest = generate(args.data_dir, spec)
    print(
        json.dumps(
            {
                "data_dir": args.data_dir,
                "history_rows": manifest["history_row_count"],
                "stream_rows": manifest["stream_row_count"],
                "batches": len(manifest["stream_batches"]),
                "duplicates": manifest["duplicate_rows"],
            },
            sort_keys=True,
        )
    )
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    cfg = JobConfig.load(args.config)
    manifest = _load_manifest(cfg)
    split_plan = _build_split_plan(cfg, manifest)
    started = time.time()

    if cfg.mode == "batch":
        report = run_batch(cfg, manifest, split_plan)
    else:
        fault_plan = FaultPlan.load(cfg.fault_config)
        engine = Engine(cfg, manifest, split_plan, fault_plan)
        resumed = engine.open()
        if resumed and engine.state.job_done:
            # Repeating a finished commit point must not re-emit anything.
            report = engine.build_report()
            engine.close()
        else:
            try:
                report = engine.run_mixed()
            except KillAfterCommit as exc:
                engine.close()
                print(json.dumps({"killed": str(exc)}, sort_keys=True))
                return 0
            engine.close()

    report["elapsed_ms"] = int((time.time() - started) * 1000)
    path = write_report(cfg.result_dir, args.report_name, report)
    print(json.dumps({"report": path, "windows": report["window_count"]}, sort_keys=True))
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="window-engine")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="generate deterministic sample data")
    gen.add_argument("--data-dir", required=True)
    gen.add_argument("--seed", type=int, default=20261119)
    gen.add_argument("--history-rows", type=int, default=120_000)
    gen.add_argument("--incremental-rows", type=int, default=40_000)
    gen.add_argument("--batches", type=int, default=128)
    gen.set_defaults(func=cmd_generate)

    run = sub.add_parser("run", help="run a window aggregation job")
    run.add_argument("--config", required=True)
    run.add_argument("--report-name", default="job_report")
    run.set_defaults(func=cmd_run)

    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except EngineError as exc:
        print(
            json.dumps(
                {"error_code": exc.code, "message": str(exc), "details": exc.details},
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return exc.exit_code


if __name__ == "__main__":
    raise SystemExit(main())
