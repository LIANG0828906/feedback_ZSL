"""Command-line entry points for replay, recovery, comparison, and GUI."""

from __future__ import annotations

import argparse
import json
import sys
import traceback
from pathlib import Path
from typing import Any

from .baseline import compare_strategies, compare_with_baseline_file
from .config import load_config
from .engine import ReplayEngine, ReplayRun, Withdrawal
from .errors import BusinessError
from .generator import PROFILE_SPECS, generate_samples
from .model import load_record
from .util import atomic_write_bytes, canonical_json


def _emit(value: Any) -> None:
    sys.stdout.write(canonical_json(value) + "\n")


def _run_trajectory(run: ReplayRun) -> bytes:
    lines = []
    for frame in run.frames:
        budget = frame["budget_usage"]
        lines.append(
            canonical_json(
                {
                    "step": frame["step"],
                    "effective_client_sequences": frame["effective_client_sequences"],
                    "effective_shots": frame["effective_shots"],
                    "state_hash": frame["state_hash"],
                    "queue_watermark": budget["queue_watermark"],
                    "peak_memory_used": budget["peak_memory_used"],
                    "crack_events": frame["crack_events"],
                }
            )
        )
    return ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")


def _parse_withdrawals(values: list[str] | None) -> tuple[Withdrawal, ...]:
    withdrawals: list[Withdrawal] = []
    for value in values or []:
        pieces = value.split(":", 3)
        if len(pieces) < 3:
            raise BusinessError(
                "E_RECORD_INVALID",
                "withdrawal must be STEP:BLOCK:AMOUNT[:REASON]",
                field="withdrawal",
            )
        try:
            step = int(pieces[0])
            block_id = int(pieces[1])
        except ValueError:
            raise BusinessError(
                "E_RECORD_INVALID",
                "withdrawal step and block must be integers",
                field="withdrawal",
            ) from None
        withdrawals.append(
            Withdrawal(
                step=step,
                block_id=block_id,
                amount=pieces[2],
                reason=pieces[3] if len(pieces) == 4 else "manual",
            )
        )
    return tuple(withdrawals)


def _clean_snapshot_directory(path: Path) -> None:
    if not path.exists():
        return
    for item in path.iterdir():
        if item.is_file() and (
            item.name.startswith("step-") or item.name.endswith(".tmp")
        ):
            item.unlink()


def _run_from_args(
    args: argparse.Namespace,
    *,
    resume: bool = False,
) -> tuple[ReplayRun, dict[str, Any]]:
    config = load_config(args.config)
    record = load_record(args.record, config)
    snapshot_dir = Path(args.snapshots) if args.snapshots else None
    if snapshot_dir is not None and not resume and getattr(args, "clean_snapshots", False):
        _clean_snapshot_directory(snapshot_dir)
    engine = ReplayEngine(
        record,
        config,
        snapshot_dir=snapshot_dir,
        resume=resume,
        withdrawals=_parse_withdrawals(getattr(args, "withdraw", None)),
        allow_conflicts=bool(getattr(args, "allow_conflicts", False)),
    )
    run = engine.run(
        stop_after_step=getattr(args, "stop_after_step", None)
    )
    summary: dict[str, Any] = {
        "ok": run.complete and not run.conflicts,
        "status": "complete" if run.complete else "interrupted",
        "match_id": run.match_id,
        "strategy": run.strategy,
        "steps_settled": len(run.frames),
        "latest_snapshot_step": run.latest_snapshot_step,
        "final_state_hash": run.final_state_hash,
        "interrupted_step": run.interrupted_step,
        "conflicts": [item.to_dict() for item in run.conflicts],
    }
    if not run.complete:
        summary["ok"] = not run.conflicts
        summary["note"] = (
            "No final replay report was written. Resume strictly from committed snapshots."
        )
        return run, summary
    output = Path(args.output)
    atomic_write_bytes(output, run.to_bytes() + b"\n")
    trajectory_path = (
        Path(args.trajectory)
        if getattr(args, "trajectory", None)
        else output.with_suffix(".trajectory.jsonl")
    )
    atomic_write_bytes(trajectory_path, _run_trajectory(run))
    summary["output"] = str(output)
    summary["trajectory"] = str(trajectory_path)
    if getattr(args, "baseline", None):
        comparison = compare_with_baseline_file(run, args.baseline)
        summary["baseline_comparison"] = comparison
        if not comparison.get("ok"):
            summary["ok"] = False
    return run, summary


def _add_run_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--record", required=True, help="local match JSON")
    parser.add_argument("--config", required=True, help="physics/budget JSON")
    parser.add_argument("--output", required=True, help="final replay result JSON")
    parser.add_argument("--trajectory", help="trajectory JSONL destination")
    parser.add_argument("--baseline", help="baseline JSON to compare after replay")
    parser.add_argument("--snapshots", help="snapshot directory")
    parser.add_argument(
        "--clean-snapshots",
        action="store_true",
        help="remove prior step snapshots before a fresh run",
    )
    parser.add_argument(
        "--stop-after-step",
        type=int,
        help="cooperatively stop after committing this step (no final report)",
    )
    parser.add_argument(
        "--allow-conflicts",
        action="store_true",
        help="continue after E_CREDIT_CONFLICT and include conflicts in the report",
    )
    parser.add_argument(
        "--withdraw",
        action="append",
        metavar="STEP:BLOCK:AMOUNT[:REASON]",
        help="inject a deterministic durability withdrawal compensation",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="cover-replay",
        description="Deterministic tactical cover replay and audit CLI",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=20261122)
    generate.add_argument(
        "--profiles",
        default=",".join(PROFILE_SPECS),
        help="comma-separated subset of small,medium,stress,conflict",
    )

    run = subparsers.add_parser("run", help="replay a local record")
    _add_run_arguments(run)

    resume = subparsers.add_parser("resume", help="resume from committed snapshots")
    _add_run_arguments(resume)

    compare = subparsers.add_parser("compare", help="replay and compare a baseline")
    _add_run_arguments(compare)

    validate = subparsers.add_parser("validate", help="validate record and configuration")
    validate.add_argument("--record", required=True)
    validate.add_argument("--config", required=True)

    strategy = subparsers.add_parser(
        "strategy-check", help="compare full-scan and windowed settlement"
    )
    strategy.add_argument("--record", required=True)
    strategy.add_argument("--config", required=True)
    strategy.add_argument("--allow-conflicts", action="store_true")

    demo = subparsers.add_parser(
        "demo", help="generate samples and run the complete acceptance suite"
    )
    demo.add_argument("--out", default="artifacts/acceptance")
    demo.add_argument("--seed", type=int, default=20261122)

    gui = subparsers.add_parser("gui", help="launch the PySide6 review desktop")
    gui.add_argument("--sample", help="sample directory containing match/config/baseline")

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            profiles = tuple(
                item.strip() for item in args.profiles.split(",") if item.strip()
            )
            manifest = generate_samples(args.out, seed=args.seed, profiles=profiles)
            _emit({"ok": True, "manifest": str(Path(args.out) / "manifest.json"), **manifest})
            return 0
        if args.command == "validate":
            config = load_config(args.config)
            record = load_record(args.record, config)
            _emit(
                {
                    "ok": True,
                    "match_id": record.match_id,
                    "cover_count": len(record.covers),
                    "shot_count": len(record.shots),
                    "physics_steps": config.physics_steps,
                    "max_workers": config.max_workers,
                    "memory_budget_per_step": config.memory_budget_per_step,
                }
            )
            return 0
        if args.command == "strategy-check":
            config = load_config(args.config)
            record = load_record(args.record, config)
            result = compare_strategies(
                record, config, allow_conflicts=args.allow_conflicts
            )
            _emit(result)
            return 0
        if args.command in {"run", "resume", "compare"}:
            _, summary = _run_from_args(
                args, resume=(args.command == "resume")
            )
            _emit(summary)
            return 0 if summary.get("ok") else 2
        if args.command == "demo":
            from .acceptance import run_acceptance

            report = run_acceptance(args.out, seed=args.seed)
            _emit(
                {
                    "ok": report["ok"],
                    "report": str(Path(args.out) / "acceptance-report.json"),
                    "checks": {
                        key: value["ok"] for key, value in report["checks"].items()
                    },
                }
            )
            return 0 if report["ok"] else 2
        if args.command == "gui":
            try:
                from .gui import launch
            except ImportError as error:
                raise BusinessError(
                    "E_NO_DISPLAY",
                    f"PySide6 desktop is unavailable: {error}",
                    suggestion="Use the run/resume/compare scripted commands.",
                ) from None
            return launch(args.sample)
    except BusinessError as error:
        _emit({"ok": False, "error": error.to_dict()})
        return 2
    except Exception as error:
        traceback.print_exc(file=sys.stderr)
        _emit(
            {
                "ok": False,
                "error": {
                    "code": "E_INTERNAL",
                    "message": str(error),
                },
            }
        )
        return 1
    return 0
