"""Command line entry point for generation, solving and verification."""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from dataclasses import replace
from pathlib import Path
from typing import Any, Sequence

from .bvh import parse_bvh_file
from .config import RunConfig, load_run_config
from .errors import MotionReviewError
from .generator import (
    DEFAULT_CASES,
    SampleCase,
    generate_dataset,
    load_case_constraints,
)
from .scheduler import ClipTask, Scheduler
from .verify import verify_dataset


def _parse_case(text: str) -> SampleCase:
    parts = text.split(":")
    if len(parts) < 3:
        raise argparse.ArgumentTypeError(
            "case must be NAME:FRAMES:JOINTS[:CONFLICT]"
        )
    conflict = False
    if len(parts) >= 4:
        conflict = parts[3].lower() in {"1", "true", "yes", "conflict"}
    return SampleCase(parts[0], int(parts[1]), int(parts[2]), conflict)


def _load_case_from_sample(
    sample_root: Path, case_name: str
) -> tuple[Any, Any]:
    manifest = json.loads(
        (sample_root / "manifest.json").read_text(encoding="utf-8")
    )
    for case in manifest["cases"]:
        if case["name"] == case_name:
            document = parse_bvh_file(
                sample_root / case["bvh"], clip_id=case_name
            )
            constraints = load_case_constraints(
                document, sample_root / case["constraints"]
            )
            return document, constraints
    raise ValueError(f"case not found in sample manifest: {case_name}")


def _config(args: argparse.Namespace) -> RunConfig:
    if args.config:
        config = load_run_config(args.config)
    else:
        config = RunConfig(
            max_parallelism=2,
            per_task_memory_bytes=64 * 1024 * 1024,
            total_memory_bytes=128 * 1024 * 1024,
            max_iterations=40,
            checkpoint_frames=32,
        )
    if args.parallelism is not None:
        config = replace(
            config, max_parallelism=int(args.parallelism)
        )
    return config


def _cmd_generate(args: argparse.Namespace) -> int:
    cases = args.case or list(DEFAULT_CASES)
    manifest = generate_dataset(
        args.out,
        cases=cases,
        seed=args.seed,
        write_baseline=not args.no_baseline,
        max_iterations=args.max_iterations,
    )
    print(
        json.dumps(
            {
                "ok": True,
                "out": str(Path(args.out).resolve()),
                "aggregate_hash": manifest["aggregate_hash"],
                "cases": [
                    {
                        "name": case["name"],
                        "frames": case["frames"],
                        "joints": case["joints"],
                        "baseline": case["baseline_aggregate_hash"],
                        "conflict_frames": case["conflict_frames"],
                    }
                    for case in manifest["cases"]
                ],
            },
            indent=2,
            ensure_ascii=False,
        )
    )
    return 0


def _cmd_validate(args: argparse.Namespace) -> int:
    document = parse_bvh_file(args.bvh)
    print(
        json.dumps(
            {
                "ok": True,
                "clip_id": document.clip_id,
                "frames": document.frame_count,
                "joints": len(document.joints),
                "channels": document.channel_count,
                "frame_time": document.frame_time,
                "content_hash": document.content_hash,
            },
            indent=2,
        )
    )
    return 0


def _cmd_solve(args: argparse.Namespace) -> int:
    if args.sample:
        document, constraints = _load_case_from_sample(
            Path(args.sample), args.case_name
        )
    else:
        if not args.bvh or not args.constraints:
            raise ValueError(
                "--bvh and --constraints are required without --sample"
        )
        document = parse_bvh_file(
            args.bvh, clip_id=args.clip_id
        )
        constraints = load_case_constraints(
            document, args.constraints
        )
    config = _config(args)
    task = ClipTask(
        document.clip_id,
        document,
        constraints,
        resume=args.resume,
    )
    with Scheduler(config, args.out) as scheduler:
        report = scheduler.run([task])
    payload = report.to_dict()
    payload["units"] = {
        "memory_bytes": "bytes",
        "residual": "constraint-specific (radians or world units)",
    }
    print(json.dumps(payload, indent=2, ensure_ascii=False))
    return 0 if report.all_completed else 2


def _cmd_verify(args: argparse.Namespace) -> int:
    config = _config(args)
    if args.output:
        output = Path(args.output)
        output.mkdir(parents=True, exist_ok=True)
        parallel = output / "parallel"
        serial = output / "serial"
        if any(
            path.is_dir()
            for path in (parallel, serial)
        ):
            raise ValueError(
                "verification output already exists; choose a new --output"
            )
        result = verify_dataset(
            args.sample,
            config=config,
            parallel_output=parallel,
            serial_output=serial,
        )
        print(json.dumps(result, indent=2, ensure_ascii=False))
        return 0 if result["ok"] else 1
    with tempfile.TemporaryDirectory(prefix="motionreview-verify-") as temporary:
        root = Path(temporary)
        result = verify_dataset(
            args.sample,
            config=config,
            parallel_output=root / "parallel",
            serial_output=root / "serial",
        )
    print(json.dumps(result, indent=2, ensure_ascii=False))
    return 0 if result["ok"] else 1


def _cmd_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(
        sample_root=args.sample,
        case_name=args.case_name,
        config_path=args.config,
        output_root=args.out,
    )


def _add_common(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--config", help="run configuration JSON"
    )
    parser.add_argument(
        "--parallelism",
        type=int,
        default=None,
        help="explicit override of config max_parallelism",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="motionreview",
        description=(
            "Deterministic BVH constraint review workbench"
        ),
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="generate deterministic local BVH samples"
    )
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=42)
    generate.add_argument(
        "--max-iterations", type=int, default=40
    )
    generate.add_argument(
        "--no-baseline",
        action="store_true",
        help="skip baseline solving for a large stress sample",
    )
    generate.add_argument(
        "--case",
        type=_parse_case,
        action="append",
        help=(
            "NAME:FRAMES:JOINTS[:CONFLICT], repeatable; "
            "defaults to bundled short/medium/conflict/long cases"
        ),
    )
    generate.set_defaults(func=_cmd_generate)

    validate = subparsers.add_parser(
        "validate", help="parse and validate a BVH file"
    )
    validate.add_argument("--bvh", required=True)
    validate.set_defaults(func=_cmd_validate)

    solve = subparsers.add_parser(
        "solve", help="run one clip through the scheduler"
    )
    solve.add_argument("--sample", help="sample dataset directory")
    solve.add_argument(
        "--case-name", default="short", help="case inside --sample"
    )
    solve.add_argument("--bvh")
    solve.add_argument("--constraints")
    solve.add_argument("--clip-id")
    solve.add_argument("--out", required=True)
    solve.add_argument("--resume", action="store_true")
    _add_common(solve)
    solve.set_defaults(func=_cmd_solve)

    verify = subparsers.add_parser(
        "verify",
        help="compare parallel and serial runs against baseline hashes",
    )
    verify.add_argument("--sample", required=True)
    verify.add_argument(
        "--output",
        help="keep verification snapshots; default uses a temp directory",
    )
    _add_common(verify)
    verify.set_defaults(func=_cmd_verify)

    gui = subparsers.add_parser(
        "gui", help="launch the desktop review workbench"
    )
    gui.add_argument("--sample")
    gui.add_argument("--case-name", default="short")
    gui.add_argument("--config")
    gui.add_argument("--out", default="gui-runs")
    gui.set_defaults(func=_cmd_gui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except MotionReviewError as exc:
        print(json.dumps(exc.to_dict(), indent=2), file=sys.stderr)
        return 4
    except (ValueError, KeyError, OSError) as exc:
        print(
            json.dumps(
                {"code": "E_USAGE", "message": str(exc)},
                indent=2,
            ),
            file=sys.stderr,
        )
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
