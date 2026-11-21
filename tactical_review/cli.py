"""Command-line entry points for sample generation and reproducible review."""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any

from .errors import ReviewError
from .model import InitialState, load_revocations, load_shots
from .reporting import build_report, compare_baseline
from .samplegen import generate_sample
from .simulation import Simulation
from .snapshot import SnapshotStore, inject_snapshot_fault


def _atomic_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2, allow_nan=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise


def _atomic_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise


def _load_sample(sample_dir: Path) -> tuple[
    InitialState,
    list[Any],
    list[Any],
    dict[str, Any],
    dict[str, Any],
    dict[str, Any],
]:
    initial = InitialState.load(str(sample_dir / "initial_state.json"))
    shots_payload, shots = load_shots(str(sample_dir / "shots.json"))
    revocations = load_revocations(str(sample_dir / "revocations.json"))
    with (sample_dir / "baseline.json").open("r", encoding="utf-8") as handle:
        baseline = json.load(handle)
    manifest_path = sample_dir / "manifest.json"
    if manifest_path.exists():
        with manifest_path.open("r", encoding="utf-8") as handle:
            manifest = json.load(handle)
    else:
        manifest = {}
    return initial, shots, revocations, shots_payload, baseline, manifest


def command_generate(args: argparse.Namespace) -> int:
    manifest = generate_sample(
        args.out,
        seed=args.seed,
        cover_count=args.covers,
        shot_count=args.shots,
        end_step=args.end_step,
        step_ms=args.step_ms,
        compensation_window=args.compensation_window,
        include_anomalies=not args.no_anomalies,
    )
    print(json.dumps(manifest, ensure_ascii=False, indent=2))
    return 0


def command_run(args: argparse.Namespace) -> int:
    sample_dir = Path(args.sample)
    initial, shots, revocations, shots_payload, baseline, manifest = _load_sample(
        sample_dir
    )
    store = SnapshotStore(args.run_dir)
    restored_from_step: int | None = None
    summaries: list[dict[str, Any]] = []
    if args.recovery_fault is not None:
        args.restore = True
    if args.restore:
        recovery = store.load_simulation(
            initial,
            shots,
            revocations,
            fault=args.recovery_fault,
            fault_step=args.recovery_fault_step,
        )
        simulation = recovery.simulation
        restored_from_step = simulation.state.step
        summaries.append(recovery.summary)
    else:
        store.reset()
        simulation = Simulation(
            initial,
            shots,
            revocations,
            progress_fault_step=args.progress_fault_step,
        )
    stop_step = initial.end_step if args.max_steps is None else min(
        initial.end_step, initial.start_step + args.max_steps - 1
    )
    try:
        while simulation.state.step < stop_step:
            summary = simulation.advance()
            store.write(simulation)
            summaries.append(summary)
            if args.step_delay:
                import time

                time.sleep(args.step_delay)
    except ReviewError as exc:
        failure = {
            "schema_version": 1,
            "fatal": True,
            "error": exc.to_dict(),
            "last_published_step": (
                store.list_steps()[-1] if store.list_steps() else 0
            ),
            "snapshot_directory": str(store.directory),
        }
        _atomic_json(Path(args.report), failure)
        print(json.dumps(failure, ensure_ascii=False, indent=2))
        return 3

    baseline_errors = compare_baseline(
        baseline,
        summaries,
        recompute_fault_step=args.recompute_fault_step,
    )
    report = build_report(
        initial=initial,
        shots_payload=shots_payload,
        simulation=simulation,
        summaries=summaries,
        baseline=baseline,
        baseline_errors=baseline_errors,
        restored_from_step=restored_from_step,
        sample_manifest=manifest,
    )
    _atomic_json(Path(args.report), report)
    trajectory_path = Path(args.trajectory)
    _atomic_text(
        trajectory_path,
        "".join(
            json.dumps(summary, ensure_ascii=False, separators=(",", ":")) + "\n"
            for summary in summaries
        ),
    )
    print(
        json.dumps(
            {
                "report": str(Path(args.report)),
                "trajectory": str(trajectory_path),
                "steps": len(summaries),
                "final_step": simulation.state.step,
                "final_hash": report["final_hash"],
                "business_codes": report["business_code_counts"],
                "fatal_codes": report["fatal_codes"],
                "initial_total": report["conservation"]["initial_total"],
                "applied_damage_total": report["conservation"][
                    "applied_damage_total"
                ],
                "remaining_total": report["conservation"]["remaining_total"],
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0 if not report["fatal_error"] else 2


def command_corrupt(args: argparse.Namespace) -> int:
    path = inject_snapshot_fault(
        SnapshotStore(args.run_dir),
        args.fault,
        step=args.step,
    )
    print(
        json.dumps(
            {"corrupted": str(path), "fault": args.fault, "step": args.step},
            ensure_ascii=False,
        )
    )
    return 0


def command_gui(args: argparse.Namespace) -> int:
    try:
        from .gui import launch_gui
    except ImportError as exc:
        print(
            json.dumps(
                {
                    "code": "E_GUI_DEPENDENCY",
                    "message": "PySide6 is required to launch the desktop workbench",
                    "detail": str(exc),
                },
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 4
    return launch_gui(
        sample_dir=args.sample,
        run_dir=args.run_dir,
        step_delay=args.step_delay,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="cover-review",
        description="Deterministic tactical-cover replay workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="generate a deterministic local sample"
    )
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=20261122)
    generate.add_argument("--covers", type=int, default=8)
    generate.add_argument("--shots", type=int, default=48)
    generate.add_argument("--end-step", type=int, default=45)
    generate.add_argument("--step-ms", type=int, default=50)
    generate.add_argument("--compensation-window", type=int, default=3)
    generate.add_argument("--no-anomalies", action="store_true")
    generate.set_defaults(func=command_generate)

    run = subparsers.add_parser("run", help="replay a sample and compare baseline")
    run.add_argument("--sample", required=True)
    run.add_argument("--run-dir", required=True)
    run.add_argument(
        "--report",
        default=None,
        help="defaults to <run-dir>/review_report.json",
    )
    run.add_argument(
        "--trajectory",
        default=None,
        help="defaults to <run-dir>/trajectory.jsonl",
    )
    run.add_argument("--restore", action="store_true")
    run.add_argument("--step-delay", type=float, default=0.0)
    run.add_argument("--max-steps", type=int, default=None)
    run.add_argument("--progress-fault-step", type=int, default=None)
    run.add_argument("--recompute-fault-step", type=int, default=None)
    run.add_argument(
        "--recovery-fault",
        choices=["missing_field", "hash_break", "durability_tamper", "prefix_digest"],
        default=None,
    )
    run.add_argument("--recovery-fault-step", type=int, default=None)
    run.set_defaults(func=command_run)

    corrupt = subparsers.add_parser(
        "corrupt", help="damage one snapshot to drill E_RECOVERY_INVALID"
    )
    corrupt.add_argument("--run-dir", required=True)
    corrupt.add_argument(
        "--fault",
        required=True,
        choices=["missing_field", "hash_break", "durability_tamper", "prefix_digest"],
    )
    corrupt.add_argument("--step", type=int, default=None)
    corrupt.set_defaults(func=command_corrupt)

    restore = subparsers.add_parser(
        "restore", help="resume from the latest snapshot and verify chain continuity"
    )
    restore.add_argument("--sample", required=True)
    restore.add_argument("--run-dir", required=True)
    restore.add_argument("--report", default=None)
    restore.add_argument("--trajectory", default=None)
    restore.add_argument("--recovery-fault", default=None)
    restore.add_argument("--recovery-fault-step", type=int, default=None)
    restore.set_defaults(
        func=command_run,
        restore=True,
        step_delay=0.0,
        max_steps=None,
        progress_fault_step=None,
        recompute_fault_step=None,
    )

    gui = subparsers.add_parser("gui", help="launch the PySide6 workbench")
    gui.add_argument("--sample", required=True)
    gui.add_argument("--run-dir", required=True)
    gui.add_argument("--step-delay", type=float, default=0.035)
    gui.set_defaults(func=command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if getattr(args, "report", None) is None and hasattr(args, "run_dir"):
        args.report = str(Path(args.run_dir) / "review_report.json")
    if getattr(args, "trajectory", None) is None and hasattr(args, "run_dir"):
        args.trajectory = str(Path(args.run_dir) / "trajectory.jsonl")
    try:
        return int(args.func(args))
    except (ReviewError, OSError, ValueError) as exc:
        if isinstance(exc, ReviewError):
            payload = exc.to_dict()
        else:
            payload = {"code": "E_IO", "message": str(exc)}
        print(json.dumps(payload, ensure_ascii=False), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

