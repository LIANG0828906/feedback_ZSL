"""Command-line entry points for generation, solving, recovery and experiments."""

from __future__ import annotations

import argparse
import json
import sys
import threading
from pathlib import Path
from typing import Any

from .errors import (
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_BVH_INVALID,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_CONSTRAINT_INVALID,
    E_IO,
    E_RECOVERY_INVALID,
    BusinessError,
)
from .experiment import run_experiment, write_json, write_trajectory
from .pipeline import SolvePipeline
from .samples import PRESETS, SampleSpec, generate_presets, generate_sample
from .solver import SolverConfig


def _add_solver_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--max-iterations", type=int, default=24)
    parser.add_argument("--damping", type=float, default=2.0e-3)
    parser.add_argument("--step-limit", type=float, default=0.65)
    parser.add_argument("--batch-frame-limit", type=int, default=600)
    parser.add_argument("--batch-complexity-limit", type=int, default=8_000_000)
    parser.add_argument("--window-size", type=int, default=96)
    parser.add_argument("--window-overlap", type=int, default=12)
    parser.add_argument("--chunk-size", type=int, default=240)
    parser.add_argument("--workers", type=int, default=1)
    parser.add_argument("--memory-budget-mb", type=int, default=512)
    parser.add_argument("--strategy", choices=["auto", "batch", "window"], default="auto")


def _config_from_args(args: argparse.Namespace) -> SolverConfig:
    return SolverConfig(
        max_iterations=args.max_iterations,
        damping=args.damping,
        step_limit=args.step_limit,
        batch_frame_limit=args.batch_frame_limit,
        batch_complexity_limit=args.batch_complexity_limit,
        window_size=args.window_size,
        window_overlap=args.window_overlap,
        chunk_size=args.chunk_size,
        workers=args.workers,
        memory_budget_bytes=max(1, args.memory_budget_mb) * 1024 * 1024,
        strategy=args.strategy,
    )


def _canonical(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )


def _cmd_generate(args: argparse.Namespace) -> int:
    out = Path(args.out)
    if args.preset == "all" and args.name is None:
        records = generate_presets(out)
        print(_canonical({
            "schema": "mocap-generation-result-v1",
            "output_dir": str(out.resolve()),
            "samples": [
                {
                    "name": record["name"],
                    "bvh": Path(record["bvh"]).name,
                    "constraints": Path(record["constraints"]).name,
                    "baseline": Path(record["baseline"]).name,
                }
                for record in records
            ],
        }))
        return 0
    preset = PRESETS.get(args.preset)
    if preset is None and args.name is None:
        raise BusinessError(E_CONFIG_INVALID, f"unknown preset {args.preset!r}")
    base = preset or SampleSpec()
    spec = SampleSpec(
        name=args.name or base.name,
        seed=args.seed if args.seed is not None else base.seed,
        frames=args.frames if args.frames is not None else base.frames,
        joints=args.joints if args.joints is not None else base.joints,
        missing_rate=(
            args.missing_rate if args.missing_rate is not None else base.missing_rate
        ),
        conflict=args.conflict if args.conflict is not None else base.conflict,
    )
    record = generate_sample(out, spec)
    print(_canonical({
        "schema": "mocap-generation-result-v1",
        "output_dir": str(out.resolve()),
        "samples": [
            {
                "name": record["name"],
                "bvh": Path(record["bvh"]).name,
                "constraints": Path(record["constraints"]).name,
                "baseline": Path(record["baseline"]).name,
            }
        ],
    }))
    return 0


def _cmd_solve(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    pipeline = SolvePipeline(
        args.bvh,
        args.constraints,
        args.snapshot_dir,
        config,
    )
    cancel_event = threading.Event()
    commits = 0

    def on_progress(event) -> None:
        nonlocal commits
        commits += 1
        if args.cancel_after_chunks and commits >= args.cancel_after_chunks:
            cancel_event.set()

    report = pipeline.run(
        resume=args.resume,
        cancel_event=cancel_event,
        progress=on_progress,
        crash_after_chunks=args.crash_after_chunks,
    )
    if args.trajectory:
        write_trajectory(
            args.trajectory,
            report,
            bvh_path=args.bvh,
            constraints_path=args.constraints,
            config=config,
        )
    if args.conflicts_out:
        write_json(
            args.conflicts_out,
            {
                "schema": "mocap-conflicts-v1",
                "strategy": report.strategy,
                "conflicts": report.conflict_reports,
            },
        )
    summary = report.summary()
    if args.summary:
        write_json(args.summary, summary)
    print(_canonical(summary))
    if report.status == "budget_exceeded":
        return 6
    if report.status == "cancelled":
        return 130
    if report.status == "failed":
        return 1
    return 0


def _cmd_recover(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    pipeline = SolvePipeline(
        args.bvh,
        args.constraints,
        args.snapshot_dir,
        config,
    )
    recovery = pipeline.recover_only()
    print(_canonical(recovery.summary()))
    return 0


def _cmd_experiment(args: argparse.Namespace) -> int:
    experiment = run_experiment(
        args.sample_dir,
        args.out,
        workers=args.workers,
    )
    print(_canonical(experiment))
    return 0


def _cmd_gui(args: argparse.Namespace) -> int:
    try:
        from .gui.app import launch
    except ImportError as exc:
        raise BusinessError(
            E_CONFIG_INVALID,
            "PyQt5/PyOpenGL GUI dependencies are not available",
            {"error": str(exc)},
        ) from exc
    return int(
        launch(
            bvh_path=args.bvh,
            constraints_path=args.constraints,
            snapshot_dir=args.snapshot_dir,
            software_gl=args.software_gl,
        )
        or 0
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mocap-workbench",
        description="Deterministic BVH constraint workbench",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", required=True)
    generate.add_argument("--preset", default="all", choices=["all", *PRESETS.keys()])
    generate.add_argument("--name")
    generate.add_argument("--seed", type=int)
    generate.add_argument("--frames", type=int)
    generate.add_argument("--joints", type=int)
    generate.add_argument("--missing-rate", type=float)
    generate.add_argument(
        "--conflict",
        choices=["none", "unreachable", "bone_length"],
    )
    generate.set_defaults(func=_cmd_generate)

    solve = sub.add_parser("solve", help="solve a BVH with local constraints")
    solve.add_argument("--bvh", required=True)
    solve.add_argument("--constraints", required=True)
    solve.add_argument("--snapshot-dir", required=True)
    solve.add_argument("--resume", action="store_true")
    solve.add_argument("--crash-after-chunks", type=int, default=0)
    solve.add_argument("--cancel-after-chunks", type=int, default=0)
    solve.add_argument("--trajectory")
    solve.add_argument("--summary")
    solve.add_argument("--conflicts-out")
    _add_solver_options(solve)
    solve.set_defaults(func=_cmd_solve)

    recover = sub.add_parser("recover", help="validate and inspect the latest snapshot chain")
    recover.add_argument("--bvh", required=True)
    recover.add_argument("--constraints", required=True)
    recover.add_argument("--snapshot-dir", required=True)
    _add_solver_options(recover)
    recover.set_defaults(func=_cmd_recover)

    experiment = sub.add_parser("experiment", help="run scripted two-scale experiments")
    experiment.add_argument("--sample-dir", required=True)
    experiment.add_argument("--out", required=True)
    experiment.add_argument("--workers", type=int, default=1)
    experiment.set_defaults(func=_cmd_experiment)

    gui = sub.add_parser("gui", help="launch the PyQt/OpenGL workbench")
    gui.add_argument("--bvh")
    gui.add_argument("--constraints")
    gui.add_argument("--snapshot-dir", default="workbench_snapshots")
    gui.add_argument("--software-gl", action="store_true")
    gui.set_defaults(func=_cmd_gui)
    return parser


_EXIT_CODES = {
    E_BUSY: 2,
    E_BVH_INVALID: 3,
    E_CONSTRAINT_INVALID: 3,
    E_RECOVERY_INVALID: 4,
    E_BUDGET_EXCEEDED: 6,
    E_CANCELLED: 130,
    E_CONFIG_INVALID: 7,
    E_IO: 8,
}


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BusinessError as exc:
        print(_canonical(exc.to_dict()), file=sys.stderr)
        return _EXIT_CODES.get(exc.code, 1)
    except KeyboardInterrupt:
        print(
            _canonical({"code": E_CANCELLED, "message": "interrupted"}),
            file=sys.stderr,
        )
        return 130
    except Exception as exc:
        print(
            _canonical(
                {
                    "code": "E_INTERNAL",
                    "message": f"{type(exc).__name__}: {exc}",
                }
            ),
            file=sys.stderr,
        )
        return 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())

