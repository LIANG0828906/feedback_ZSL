"""Command-line entry points for generation, fitting, verification and GUI."""

from __future__ import annotations

import argparse
import json
import tempfile
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .checkpoint import CheckpointManager
from .engine import FittingConfig, FittingSession, SimulatedTermination
from .errors import E_BUDGET_EXCEEDED, E_REDUCE_DIVERGE, AppError
from .graph import IncrementalEvaluator
from .params import GROUPS
from .reduction import compute_gradients, gradient_byte_identity, results_equivalent
from .samples import generate_matrix, generate_sample
from .scene import load_scene
from .scheduler import BudgetConfig, ResourcePool


def _print_json(payload: object) -> None:
    print(
        json.dumps(
            payload,
            sort_keys=True,
            indent=2,
            ensure_ascii=True,
            allow_nan=False,
        )
    )


def _write_report(path: str | Path, payload: object) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    encoded = (
        json.dumps(
            payload,
            sort_keys=True,
            indent=2,
            ensure_ascii=True,
            allow_nan=False,
        ).encode("utf-8")
        + b"\n"
    )
    temporary = target.with_name(f".{target.name}.tmp")
    temporary.write_bytes(encoded)
    temporary.replace(target)


def _parse_ints(value: str) -> list[int]:
    result = [int(part) for part in value.split(",") if part.strip()]
    if not result:
        raise argparse.ArgumentTypeError("expected at least one integer")
    return result


def _groups(value: str) -> tuple[str, ...]:
    result = tuple(part.strip() for part in value.split(",") if part.strip())
    unknown = set(result) - set(GROUPS)
    if unknown:
        raise argparse.ArgumentTypeError(f"unknown groups: {sorted(unknown)}")
    return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="inverse-imaging",
        description="Deterministic local inverse imaging workstation",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=7)
    generate.add_argument("--width", type=int, default=24)
    generate.add_argument("--height", type=int, default=18)
    generate.add_argument("--spheres", type=int, default=2)
    generate.add_argument("--softness", type=float, default=0.075)
    generate.add_argument("--noise", type=float, default=0.002)
    generate.add_argument("--name", default="sample")
    generate.add_argument("--matrix", action="store_true")
    generate.add_argument("--include-large", action="store_true")

    fit = subparsers.add_parser("fit", help="run one incrementally checkpointed fit")
    fit.add_argument("--scene", required=True)
    fit.add_argument("--steps", type=int, default=6)
    fit.add_argument("--lr", type=float, default=0.02)
    fit.add_argument("--groups", type=_groups, default=GROUPS)
    fit.add_argument("--chunk-size", type=int, default=8)
    fit.add_argument("--parallel", type=int, default=2)
    fit.add_argument("--budget-mb", type=float, default=512.0)
    fit.add_argument("--budget-parallel", type=int, default=1)
    fit.add_argument("--task-quota-mb", type=float, default=None)
    fit.add_argument("--checkpoint-dir", default=None)
    fit.add_argument("--no-verify-incremental", action="store_true")
    fit.add_argument("--terminate-after-step", type=int, default=None)
    fit.add_argument("--hard-exit-after-step", type=int, default=None)

    verify = subparsers.add_parser(
        "verify", help="verify reduction, stale-cache denial, recovery and budgets"
    )
    verify.add_argument("--scene", required=True)
    verify.add_argument("--parallels", type=_parse_ints, default=[1, 2, 4])
    verify.add_argument("--chunks", type=_parse_ints, default=[4, 8, 16])
    verify.add_argument("--steps", type=int, default=4)
    verify.add_argument("--report", default=None)

    demo = subparsers.add_parser(
        "demo", help="generate a sample matrix and run the local acceptance suite"
    )
    demo.add_argument("--workdir", default="demo_run")
    demo.add_argument("--seed", type=int, default=7)
    demo.add_argument("--include-large", action="store_true")
    demo.add_argument("--report", default=None)

    gui = subparsers.add_parser("gui", help="launch the PyQt5 desktop interface")
    gui.add_argument("--scene", default=None)
    gui.add_argument("--checkpoint-dir", default=None)
    return parser


def _cmd_generate(args: argparse.Namespace) -> int:
    if args.matrix:
        result = generate_matrix(
            args.out,
            seed=args.seed,
            include_large=args.include_large,
        )
    else:
        result = generate_sample(
            args.out,
            seed=args.seed,
            width=args.width,
            height=args.height,
            sphere_count=args.spheres,
            softness=args.softness,
            noise=args.noise,
            name=args.name,
        )
    _print_json(result)
    return 0


def _cmd_fit(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    checkpoint_dir = (
        Path(args.checkpoint_dir).resolve()
        if args.checkpoint_dir
        else scene.path.parent / "checkpoint"
    )
    pool = ResourcePool(
        BudgetConfig(
            max_parallel=args.budget_parallel,
            max_memory_mb=args.budget_mb,
            default_task_quota_mb=args.task_quota_mb,
        )
    )
    holder: dict[str, Any] = {}

    def run(job: Any) -> dict[str, Any]:
        config = FittingConfig(
            steps=args.steps,
            learning_rate=args.lr,
            active_groups=args.groups,
            chunk_size=job.chunk_size,
            parallel=args.parallel,
            verify_incremental=not args.no_verify_incremental,
            checkpoint_dir=str(checkpoint_dir),
            termination_after_step=args.terminate_after_step,
            hard_exit_after_step=args.hard_exit_after_step,
        )
        session = FittingSession(scene, config)
        outcome = session.run_sync()
        holder["session"] = session
        holder["outcome"] = outcome
        return {
            "status": outcome.status,
            "steps_completed": outcome.steps_completed,
            "latest_handle": outcome.latest.handle if outcome.latest else None,
            "error": outcome.error,
        }

    try:
        job = pool.submit(
            run,
            f"fit:{scene.scene_id[:12]}",
            chunk_size=args.chunk_size,
            metadata={
                "scene_id": scene.scene_id,
                "checkpoint_dir": str(checkpoint_dir),
            },
        )
    except AppError as exc:
        if exc.code != E_BUDGET_EXCEEDED:
            raise
        _print_json({"ok": False, "error": exc.to_dict(), "events": pool.events()})
        return 1
    jobs = pool.run_all()
    scheduler_path = Path(checkpoint_dir) / "scheduler.jsonl"
    scheduler_hash = pool.write_events(scheduler_path)
    outcome = holder.get("outcome")
    session = holder.get("session")
    snapshot = session.latest_snapshot if session is not None else None
    payload = {
        "ok": bool(outcome and outcome.status in ("completed", "cancelled")),
        "job": job.public(),
        "scheduler_events": pool.events(),
        "outcome": {
            "status": outcome.status,
            "steps_completed": outcome.steps_completed,
            "error": outcome.error,
        }
        if outcome
        else None,
        "snapshot": snapshot.to_dict() if snapshot else None,
        "degradations": list(session.degradations) if session else [],
        "trajectory": str(Path(checkpoint_dir) / "trajectory.jsonl"),
        "scheduler_log": str(scheduler_path),
        "scheduler_log_sha256": scheduler_hash,
        "checkpoint_dir": str(Path(checkpoint_dir)),
        "job_count": len(jobs),
    }
    _print_json(payload)
    return 0 if payload["ok"] else 1


def _first_difference(left: np.ndarray, right: np.ndarray) -> int:
    indices = np.nonzero(left != right)[0]
    return int(indices[0]) if len(indices) else 0


def _assert_close(
    actual: Any,
    reference: Any,
    *,
    label: str,
    atol: float = 1.0e-12,
    rtol: float = 1.0e-10,
) -> None:
    if not np.allclose(actual, reference, atol=atol, rtol=rtol):
        raise AppError(
            E_REDUCE_DIVERGE,
            "gradient reduction is not invariant",
            label,
            {"first_difference": _first_difference(actual, reference)},
        )


def verify_scene(
    scene_path: str,
    *,
    parallels: Iterable[int] = (1, 2, 4),
    chunk_sizes: Iterable[int] = (4, 8, 16),
    recovery_steps: int = 4,
) -> dict[str, Any]:
    scene = load_scene(scene_path)
    reference = compute_gradients(
        scene.params,
        scene,
        chunk_size=min(chunk_sizes),
        parallel=1,
    )
    reduction_rows: list[dict[str, Any]] = []
    for chunk_size in chunk_sizes:
        for parallel in parallels:
            result = compute_gradients(
                scene.params,
                scene,
                chunk_size=chunk_size,
                parallel=parallel,
            )
            maximum = {
                group: float(
                    np.max(np.abs(result.gradients[group] - reference.gradients[group]))
                )
                for group in GROUPS
            }
            for group in GROUPS:
                _assert_close(
                    result.gradients[group],
                    reference.gradients[group],
                    label=f"{group}@{chunk_size}x{parallel}",
                )
            if abs(result.loss - reference.loss) > 1.0e-12 + 1.0e-10 * abs(reference.loss):
                raise AppError(
                    E_REDUCE_DIVERGE,
                    "chunk loss differs from reference",
                    "loss",
                    {"chunk_size": chunk_size, "parallel": parallel},
                )
            reduction_rows.append(
                {
                    "chunk_size": chunk_size,
                    "parallel": parallel,
                    "loss": result.loss,
                    "gradients_sha256": result.gradients_sha256,
                    "max_abs_difference": maximum,
                    "reduction_order": [list(item) for item in result.reduction_order],
                }
            )

    evaluator = IncrementalEvaluator(scene)
    initial = evaluator.evaluate(
        scene.params,
        modified_groups=GROUPS,
        chunk_size=min(chunk_sizes),
        parallel=1,
    )
    scene.params.values["material"]["ambient"] += 0.003
    scene.params.bump(["material"])
    incremental = evaluator.evaluate(
        scene.params,
        modified_groups=("material",),
        chunk_size=min(chunk_sizes),
        parallel=min(parallels),
    )
    fresh = compute_gradients(
        scene.params,
        scene,
        chunk_size=min(chunk_sizes),
        parallel=1,
    )
    equivalent, difference = results_equivalent(incremental, fresh)
    if not equivalent:
        raise AppError(
            E_REDUCE_DIVERGE,
            "incremental result differs from full recompute",
            str(difference),
        )

    # Simulate a caller that changed an upstream parameter but forgot to mark
    # the dependency graph invalid. The version/content signature must deny it.
    scene.params.values["material"]["ambient"] += 0.003
    scene.params.bump(["material"])
    stale = evaluator.evaluate(
        scene.params,
        modified_groups=(),
        chunk_size=min(chunk_sizes),
        parallel=1,
    )
    if not stale.degraded or not str(stale.downgrade_reason).startswith(
        "stale_dependency:"
    ):
        raise AppError(
            E_REDUCE_DIVERGE,
            "stale upstream cache was not denied",
            "cache",
        )
    stale_fresh = compute_gradients(
        scene.params,
        scene,
        chunk_size=min(chunk_sizes),
        parallel=1,
    )
    if not gradient_byte_identity(stale, stale_fresh):
        raise AppError(
            E_REDUCE_DIVERGE,
            "denied cache did not publish byte-identical full gradients",
            "cache",
        )

    # Corrupt an unaffected node value without changing its recorded hash.
    ray_key = next(
        key for key, _ in sorted(evaluator._cache.items()) if key[1] == "camera_rays"
    )
    entry = evaluator._cache[ray_key]
    origins, directions = entry.value
    corrupted_directions = directions.copy()
    corrupted_directions.flat[0] += 1.0
    entry.value = (origins.copy(), corrupted_directions)
    scene.params.values["light"]["intensity"] += 0.002
    scene.params.bump(["light"])
    corrupt_denied = evaluator.evaluate(
        scene.params,
        modified_groups=("light",),
        chunk_size=min(chunk_sizes),
        parallel=1,
    )
    if not corrupt_denied.degraded or not str(
        corrupt_denied.downgrade_reason
    ).startswith("cache_hash_mismatch:"):
        raise AppError(
            E_REDUCE_DIVERGE,
            "cache content hash mismatch was not denied",
            "cache",
        )

    recovery = _verify_recovery(scene_path, steps=max(1, recovery_steps))
    if not recovery["trajectory_identical"] or not recovery["checkpoints_identical"]:
        raise AppError(
            E_REDUCE_DIVERGE,
            "recovered trajectory or checkpoints differ",
            "recovery",
        )
    budget = _verify_budget()
    return {
        "ok": True,
        "scene_id": scene.scene_id,
        "reference_gradients_sha256": reference.gradients_sha256,
        "reductions": reduction_rows,
        "incremental": {
            "affected_nodes": list(incremental.affected_nodes),
            "reused_nodes": list(incremental.reused_nodes),
            "recomputed_nodes": list(incremental.recomputed_nodes),
            "degraded": incremental.degraded,
            "verification": incremental.verification,
            "fresh_gradients_sha256": fresh.gradients_sha256,
        },
        "stale_denial": {
            "degraded": stale.degraded,
            "reason": stale.downgrade_reason,
            "byte_identical_to_fresh": True,
        },
        "cache_hash_denial": {
            "degraded": corrupt_denied.degraded,
            "reason": corrupt_denied.downgrade_reason,
        },
        "recovery": recovery,
        "budget": budget,
    }


def _verify_recovery(scene_path: str, *, steps: int) -> dict[str, Any]:
    scene = load_scene(scene_path)
    with tempfile.TemporaryDirectory(prefix="inverse-imaging-recovery-") as temporary:
        root = Path(temporary)
        uninterrupted_dir = root / "uninterrupted"
        interrupted_dir = root / "interrupted"

        def config(directory: Path, terminate_after: int | None = None) -> FittingConfig:
            return FittingConfig(
                steps=steps,
                learning_rate=0.01,
                active_groups=GROUPS,
                chunk_size=min(4, scene.width, scene.height),
                parallel=2,
                verify_incremental=True,
                checkpoint_dir=str(directory),
                termination_after_step=terminate_after,
            )

        uninterrupted = FittingSession(scene, config(uninterrupted_dir)).run_sync()
        first = FittingSession(
            scene, config(interrupted_dir, terminate_after=0)
        ).run_sync()
        resumed = FittingSession(scene, config(interrupted_dir)).run_sync()
        trajectory_a = (uninterrupted_dir / "trajectory.jsonl").read_bytes()
        trajectory_b = (interrupted_dir / "trajectory.jsonl").read_bytes()
        checkpoints_a = {
            path.name: path.read_bytes()
            for path in sorted((uninterrupted_dir / "checkpoints").iterdir())
        }
        checkpoints_b = {
            path.name: path.read_bytes()
            for path in sorted((interrupted_dir / "checkpoints").iterdir())
        }
        latest = CheckpointManager(interrupted_dir, scene.scene_id).load_latest()
        return {
            "steps": steps,
            "uninterrupted_status": uninterrupted.status,
            "interrupted_status": first.status,
            "resumed_status": resumed.status,
            "trajectory_identical": trajectory_a == trajectory_b,
            "checkpoints_identical": checkpoints_a == checkpoints_b,
            "latest_step": latest.step if latest else None,
            "latest_hash": latest.self_hash if latest else None,
            "trajectory_sha256": __import__("hashlib").sha256(trajectory_a).hexdigest(),
        }


def _verify_budget() -> dict[str, Any]:
    pool = ResourcePool(
        BudgetConfig(
            max_parallel=2,
            max_memory_mb=18.0,
            default_task_quota_mb=12.0,
        )
    )
    completed: list[str] = []

    def job(name: str):
        def run() -> str:
            completed.append(name)
            return name

        return run

    pool.submit(job("first"), "first", chunk_size=2)
    pool.submit(job("second"), "second", chunk_size=2)
    pool.submit(
        job("third"),
        "third",
        chunk_size=2,
        requested_memory_mb=10.0,
        task_quota_mb=12.0,
    )
    jobs = pool.run_all()
    rejected = False
    try:
        pool.submit(
            job("too-large"),
            "too-large",
            chunk_size=1,
            requested_memory_mb=64.0,
            task_quota_mb=8.0,
        )
    except AppError as exc:
        rejected = exc.code == E_BUDGET_EXCEEDED
    return {
        "stable_sequence": [job.sequence for job in jobs],
        "statuses": [job.status for job in jobs],
        "completed": completed,
        "rejected_budget_case": rejected,
        "reserved_memory_mb": pool.reserved_memory_mb,
    }


def _cmd_verify(args: argparse.Namespace) -> int:
    try:
        result = verify_scene(
            args.scene,
            parallels=args.parallels,
            chunk_sizes=args.chunks,
            recovery_steps=args.steps,
        )
    except AppError as exc:
        payload = {"ok": False, "error": exc.to_dict()}
        if args.report:
            _write_report(args.report, payload)
        _print_json(payload)
        return 1
    if args.report:
        _write_report(args.report, result)
    _print_json(result)
    return 0


def _cmd_demo(args: argparse.Namespace) -> int:
    workdir = Path(args.workdir).resolve()
    matrix = generate_matrix(
        workdir / "samples",
        seed=args.seed,
        include_large=args.include_large,
    )
    selected = matrix["samples"][1] if len(matrix["samples"]) > 1 else matrix["samples"][0]
    verification = verify_scene(
        selected["scene"],
        parallels=(1, 2, 4),
        chunk_sizes=(2, 4, 8),
        recovery_steps=4,
    )
    report = {
        "format": "inverse-imaging-demo-report",
        "format_version": 1,
        "seed": args.seed,
        "manifest": str(workdir / "samples" / "manifest.json"),
        "manifest_hash": matrix["manifest_hash"],
        "selected_scene": selected["scene"],
        "verification": verification,
        "ok": bool(verification["ok"]),
    }
    report_path = Path(args.report) if args.report else workdir / "demo_report.json"
    _write_report(report_path, report)
    _print_json({"ok": report["ok"], "report": str(report_path)})
    return 0 if report["ok"] else 1


def _cmd_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(scene_path=args.scene, checkpoint_dir=args.checkpoint_dir)


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            return _cmd_generate(args)
        if args.command == "fit":
            return _cmd_fit(args)
        if args.command == "verify":
            return _cmd_verify(args)
        if args.command == "demo":
            return _cmd_demo(args)
        if args.command == "gui":
            return _cmd_gui(args)
    except AppError as exc:
        _print_json({"ok": False, "error": exc.to_dict()})
        return 1
    parser.error("unknown command")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())

