from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path
from typing import Any

from .conversion import export_score, parse_score
from .errors import EngineError
from .performance_validation import ValidationTolerance, validate_performance
from .publish import VersionedPublisher
from .recovery import RecoveryManager, SimulatedCrash
from .sample_generator import generate_samples
from .solver import PerformanceSolver, sequence_bytes
from .util import atomic_write_json, canonical_json_text


def _emit(value: Any) -> None:
    sys.stdout.write(canonical_json_text(value))


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="score-engine",
        description="Deterministic digital-score expression engine",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic local samples and baselines")
    generate.add_argument("--out", default="artifacts/generated")
    generate.add_argument("--seed", type=int, default=20261122)

    solve = sub.add_parser("solve", help="parse, solve, validate and publish a score")
    solve.add_argument("--score", required=True)
    solve.add_argument("--output")
    solve.add_argument("--manifest-dir")
    solve.add_argument("--version", type=int, default=1)
    solve.add_argument("--timing-tolerance-ms", type=float, default=2.0)
    solve.add_argument("--velocity-tolerance", type=float, default=1.0)

    validate = sub.add_parser("validate", help="validate a performance sequence against score truth")
    validate.add_argument("--score", required=True)
    validate.add_argument("--sequence", required=True)
    validate.add_argument("--timing-tolerance-ms", type=float, default=2.0)
    validate.add_argument("--velocity-tolerance", type=float, default=1.0)

    convert = sub.add_parser("convert", help="upgrade or downgrade between score versions")
    convert.add_argument("--score", required=True)
    convert.add_argument("--to", required=True, type=int, choices=[1, 2, 3])
    convert.add_argument("--output")
    convert.add_argument("--allow-loss", action="store_true")

    recover = sub.add_parser("recover-demo", help="demonstrate crash recovery from per-voice checkpoints")
    recover.add_argument("--score", required=True)
    recover.add_argument("--out", default="artifacts/recovery-demo")
    recover.add_argument("--crash-after", type=int, default=2)
    recover.add_argument("--parameter-version", type=int, default=7)

    gui = sub.add_parser("gui", help="launch the PySide6 performance dashboard")
    gui.add_argument("--score")
    gui.add_argument("--smoke", action="store_true")

    demo = sub.add_parser("demo", help="run the complete local acceptance pipeline")
    demo.add_argument("--out", default="artifacts/acceptance")
    demo.add_argument("--seed", type=int, default=20261122)
    return parser


def _cmd_generate(args: argparse.Namespace) -> int:
    result = generate_samples(args.out, seed=args.seed)
    _emit(result)
    return 0


def _cmd_solve(args: argparse.Namespace) -> int:
    score = parse_score(args.score)
    solver = PerformanceSolver()
    sequence = solver.solve(score)
    tolerance = ValidationTolerance(
        timing_ms=args.timing_tolerance_ms,
        velocity=args.velocity_tolerance,
    )
    validation = validate_performance(score, sequence, tolerance=tolerance)
    publisher = VersionedPublisher(args.manifest_dir)
    snapshot = publisher.publish(sequence, version=args.version)
    if args.output:
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(sequence_bytes(sequence))
    _emit(
        {
            "code": "OK",
            "score": args.score,
            "source_version": score.source_version,
            "part_count": len(score.parts),
            "note_count": score.note_count(),
            "sequence_version": snapshot.version,
            "content_hash": snapshot.content_hash,
            "validation": validation["summary"],
            "maximum_deviation_note": validation["maximum_deviation_note"],
            "violations": validation["violations"],
        }
    )
    return 0


def _cmd_validate(args: argparse.Namespace) -> int:
    score = parse_score(args.score)
    with Path(args.sequence).open("r", encoding="utf-8") as handle:
        sequence = json.load(handle)
    tolerance = ValidationTolerance(
        timing_ms=args.timing_tolerance_ms,
        velocity=args.velocity_tolerance,
    )
    report = validate_performance(score, sequence, tolerance=tolerance)
    _emit(report)
    return 0 if report["summary"]["within_tolerance"] else 3


def _cmd_convert(args: argparse.Namespace) -> int:
    score = parse_score(args.score)
    result = export_score(score, args.to, allow_loss=args.allow_loss)
    if args.output:
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        atomic_write_json(output, result.payload)
    _emit(
        {
            **result.report(),
            "output": args.output,
        }
    )
    return 0


def _cmd_recover(args: argparse.Namespace) -> int:
    score = parse_score(args.score)
    root = Path(args.out)
    recovery_dir = root / "recovery"
    if recovery_dir.exists():
        shutil.rmtree(recovery_dir)
    root.mkdir(parents=True, exist_ok=True)
    manager = RecoveryManager(
        recovery_dir,
        score,
        parameter_version=args.parameter_version,
        sequence_version=1,
    )
    crashed = False
    try:
        manager.resume(crash_after_new_parts=args.crash_after)
    except SimulatedCrash:
        crashed = True
    result = manager.resume()
    sequence_path = root / "recovered.performance.json"
    sequence_path.write_bytes(sequence_bytes(result.sequence))
    fresh = PerformanceSolver().solve(score)
    report = {
        **result.report(),
        "simulated_crash_observed": crashed,
        "equivalent_to_fresh_solve": sequence_bytes(fresh) == sequence_bytes(result.sequence),
        "sequence_path": str(sequence_path),
    }
    atomic_write_json(root / "recovery-report.json", report)
    _emit(report)
    return 0


def _cmd_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(score_path=args.score, smoke=args.smoke)


def _cmd_demo(args: argparse.Namespace) -> int:
    result = generate_samples(args.out, seed=args.seed)
    _emit(
        {
            "code": "OK",
            "acceptance_root": str(Path(args.out)),
            "trace_path": result["trace_path"],
            "trace_records": result["trace_records"],
            "baseline_manifest": str(Path(args.out) / "manifests" / "baseline-manifest.json"),
            "seed": args.seed,
        }
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    commands = {
        "generate": _cmd_generate,
        "solve": _cmd_solve,
        "validate": _cmd_validate,
        "convert": _cmd_convert,
        "recover-demo": _cmd_recover,
        "gui": _cmd_gui,
        "demo": _cmd_demo,
    }
    try:
        return commands[args.command](args)
    except EngineError as exc:
        _emit(exc.to_dict())
        return 2
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        _emit(
            {
                "code": "E_INTERNAL",
                "message": str(exc),
                "type": type(exc).__name__,
            }
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

