"""Command-line entry point."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from .config import config_from_dict, load_config
from .errors import EXIT_CODES, ExecutorError
from .job import JobOptions, run_job


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Run a MySQL/PostgreSQL dialect query on local CSV/Parquet data"
    )
    parser.add_argument("--query", required=True, help="SQL file")
    parser.add_argument("--dialect", choices=["mysql", "postgres", "postgresql", "pg"])
    parser.add_argument("--data-dir", default="data", help="schema.json and data files")
    parser.add_argument("--config", help="JSON executor configuration")
    parser.add_argument("--output", default="runs/result.csv", help="result path")
    parser.add_argument("--report", default="runs/report.json", help="report path")
    parser.add_argument("--run-id", help="stable temporary workspace id")
    parser.add_argument("--resume", action="store_true", help="resume completed partitions")
    parser.add_argument("--partitions", type=int)
    parser.add_argument("--batch-rows", type=int)
    parser.add_argument("--stream-batch-rows", type=int)
    parser.add_argument("--max-workers", type=int)
    parser.add_argument("--max-retries", type=int)
    parser.add_argument("--stream-mode", choices=["bounded", "incremental"])
    parser.add_argument("--fault-partition", help="partition id, e.g. join_01:p0001")
    parser.add_argument("--fault-batch", type=int, default=0)
    parser.add_argument("--no-fault", action="store_true")
    parser.add_argument("--force-spill", action="store_true")
    parser.add_argument(
        "--result-format", choices=["csv", "parquet"], dest="result_format"
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        config = load_config(args.config)
        overrides = {}
        for name in (
            "partitions",
            "batch_rows",
            "stream_batch_rows",
            "max_workers",
            "max_retries",
            "stream_mode",
            "force_spill",
            "result_format",
        ):
            value = getattr(args, name)
            if value is not None and value is not False:
                overrides[name] = value
        if args.no_fault:
            overrides["fault_injection"] = None
        elif args.fault_partition:
            overrides["fault_injection"] = {
                "partition_id": args.fault_partition,
                "batch": args.fault_batch,
            }
        if overrides:
            config = config_from_dict({**config.raw, **overrides})
        report, code = run_job(
            JobOptions(
                query_path=Path(args.query),
                data_dir=Path(args.data_dir),
                result_path=Path(args.output),
                report_path=Path(args.report),
                dialect=args.dialect,
                config=config,
                run_id=args.run_id,
                resume=args.resume,
            )
        )
        if code == 0:
            result = report.get("result", {})
            print(
                f"success rows={result.get('rows')} "
                f"sha256={result.get('sha256')} report={args.report}"
            )
        else:
            print(
                f"failed code={report.get('error_code')} "
                f"message={report.get('error', {}).get('message')}",
                file=sys.stderr,
            )
        return code
    except ExecutorError as exc:
        print(f"{exc.code}: {exc.message}", file=sys.stderr)
        return EXIT_CODES.get(exc.code, 2)


if __name__ == "__main__":
    raise SystemExit(main())

