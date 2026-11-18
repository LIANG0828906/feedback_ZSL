"""Command-line entry point."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from .engine import BackfillEngine, EngineConfig
from .errors import EngineError
from .generator import GeneratorConfig, generate_dataset
from .util import write_json


def _add_common_run_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--data", type=Path, required=True, help="input data directory")
    parser.add_argument("--state", type=Path, required=True, help="SQLite state path")
    parser.add_argument("--out", type=Path, required=True, help="report/output directory")
    parser.add_argument("--slots", type=int, default=6, help="logical processing slots")
    parser.add_argument(
        "--workers", type=int, default=2, help="OS worker processes (hard capped at 2)"
    )
    parser.add_argument(
        "--allowed-lateness-seconds",
        type=int,
        default=60,
        help="watermark = declared_event_time - this tolerance",
    )
    parser.add_argument(
        "--skew-threshold", type=float, default=0.40, help="maximum per-slot event share"
    )
    parser.add_argument("--spill-mb", type=float, default=128.0)
    parser.add_argument("--memory-limit-mb", type=float, default=900.0)
    parser.add_argument("--temp-dir", type=Path, default=None)
    parser.add_argument(
        "--terminate-after-batch",
        type=int,
        default=None,
        help="fault injection: hard-exit immediately after this committed batch",
    )
    parser.add_argument("--metrics-port", type=int, default=None)
    parser.add_argument(
        "--reset-state",
        action="store_true",
        help="explicitly delete an existing/quarantined state database",
    )
    parser.add_argument("--quiet", action="store_true")


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="minute-backfill",
        description="Offline minute-window payment/refund backfill engine",
    )
    parser.add_argument("--version", action="version", version="minute-backfill 1.0.0")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic local input data")
    generate.add_argument("--out", type=Path, required=True)
    generate.add_argument("--seed", type=int, default=42)
    generate.add_argument("--payment-rows", type=int, default=100_000)
    generate.add_argument("--refund-rows", type=int, default=40_000)
    generate.add_argument("--batches", type=int, default=128)
    generate.add_argument("--minutes", type=int, default=128)
    generate.add_argument("--duplicate-rate", type=float, default=0.05)
    generate.add_argument("--delay-rate", type=float, default=0.03)
    generate.add_argument("--late-min-seconds", type=int, default=30)
    generate.add_argument("--late-max-seconds", type=int, default=900)
    generate.add_argument("--out-of-order-rate", type=float, default=0.20)
    generate.add_argument(
        "--skew-share",
        type=float,
        default=0.0,
        help="share of raw payment rows assigned to one hot payment_id",
    )
    generate.add_argument("--hot-payment-id", default="P-HOT-0001")
    generate.add_argument("--start-time", default="2026-11-01T00:00:00Z")
    generate.add_argument("--currency", default="CNY")
    generate.add_argument("--force", action="store_true")

    run = sub.add_parser("run", help="run or resume a backfill job")
    _add_common_run_args(run)

    inspect = sub.add_parser("inspect", help="print state summary without processing input")
    inspect.add_argument("--data", type=Path, required=True)
    inspect.add_argument("--state", type=Path, required=True)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    if args.command == "generate":
        try:
            report = generate_dataset(
                GeneratorConfig(
                    out_dir=args.out,
                    seed=args.seed,
                    payment_rows=args.payment_rows,
                    refund_rows=args.refund_rows,
                    batches=args.batches,
                    minutes=args.minutes,
                    duplicate_rate=args.duplicate_rate,
                    delay_rate=args.delay_rate,
                    late_min_seconds=args.late_min_seconds,
                    late_max_seconds=args.late_max_seconds,
                    out_of_order_rate=args.out_of_order_rate,
                    skew_share=args.skew_share,
                    hot_payment_id=args.hot_payment_id,
                    start_time=args.start_time,
                    currency=args.currency,
                    force=args.force,
                )
            )
        except Exception as exc:
            error = EngineError("E_INPUT", f"dataset generation failed: {exc}")
            _print_error(error)
            return error.exit_code
        print(json.dumps(report, ensure_ascii=True, sort_keys=True))
        return 0

    if args.command == "inspect":
        from .inputs import InputCatalog
        from .state import StateStore

        try:
            catalog = InputCatalog.load(args.data)
            with StateStore(args.state) as state:
                payload = {
                    "input": {
                        "payment_shards": len(catalog.shards),
                        "refund_batches": len(catalog.batches),
                    },
                    "state": state.state_summary(),
                    "recovery": state.recovery_report(),
                }
        except EngineError as exc:
            _print_error(exc)
            return exc.exit_code
        print(json.dumps(payload, ensure_ascii=True, indent=2, sort_keys=True))
        return 0

    config = EngineConfig(
        data_dir=args.data,
        state_db=args.state,
        out_dir=args.out,
        slots=args.slots,
        workers=args.workers,
        allowed_lateness_seconds=args.allowed_lateness_seconds,
        skew_threshold=args.skew_threshold,
        spill_mb=args.spill_mb,
        memory_limit_mb=args.memory_limit_mb,
        temp_dir=args.temp_dir,
        terminate_after_batch=args.terminate_after_batch,
        metrics_port=args.metrics_port,
        reset_state=args.reset_state,
    )
    engine: BackfillEngine | None = None
    try:
        engine = BackfillEngine(config)
        report = engine.run()
    except EngineError as exc:
        if engine is None:
            _write_early_error(args.out, exc)
        _print_error(exc)
        return exc.exit_code
    except ValueError as exc:
        error = EngineError("E_INPUT", f"invalid configuration: {exc}")
        _write_early_error(args.out, error)
        _print_error(error)
        return error.exit_code
    except Exception as exc:
        error = EngineError("E_INTERNAL", f"unexpected failure: {exc}")
        _write_early_error(args.out, error)
        _print_error(error)
        return error.exit_code
    if not args.quiet:
        summary = {
            "status": report["status"],
            "report": str((args.out / "report.json").resolve()),
            "windows": len(report["windows"]),
            "payment_shards": report["input"]["payment_shard_count"],
            "refund_batches": report["input"]["refund_batch_count"],
            "dedup_dropped": report["dedup"]["dropped"],
            "late_events": report["late"]["total"],
            "recovery_count": report["recovery"]["count"],
            "skew_max_share": report["skew"]["max_share"],
            "spill_bytes": report["resources"]["spill_bytes"],
            "peak_memory_bytes": report["resources"]["peak_memory_bytes"],
            "duration_seconds": report["timing"]["duration_seconds"],
        }
        print(json.dumps(summary, ensure_ascii=True, sort_keys=True))
    return 0


def _print_error(error: EngineError) -> None:
    print(json.dumps({"error": error.to_dict()}, ensure_ascii=True, sort_keys=True), file=sys.stderr)


def _write_early_error(out_dir: Path, error: EngineError) -> None:
    try:
        write_json(
            out_dir / "last_error.json",
            {"status": "failed", "error": error.to_dict()},
        )
    except Exception:
        pass


if __name__ == "__main__":
    raise SystemExit(main())
