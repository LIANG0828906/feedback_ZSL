"""Scripted deterministic experiments and trajectory logging."""

from __future__ import annotations

import json
from collections.abc import Mapping
from dataclasses import replace
from pathlib import Path
from typing import Any

import numpy as np

from .bvh import parse_bvh
from .constraints import load_constraints
from .math3d import canonical_json_bytes, hash_file, hash_text
from .pipeline import RunReport, SolvePipeline
from .solver import SolveCache, SolverConfig, solve_frames


def _write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(payload)


def write_json(path: str | Path, value: Mapping[str, Any]) -> None:
    target = Path(path)
    _write_bytes(
        target,
        canonical_json_bytes(value) + b"\n",
    )


def write_trajectory(
    path: str | Path,
    report: RunReport,
    *,
    bvh_path: str | Path,
    constraints_path: str | Path,
    config: SolverConfig,
) -> str:
    header = {
        "record_type": "run",
        "schema": "mocap-trajectory-v1",
        "bvh_sha256": hash_file(str(bvh_path)),
        "constraints_sha256": hash_file(str(constraints_path)),
        "frame_count": report.frame_count,
        "strategy": report.strategy,
        "constraints_revision": report.constraints_revision,
        "constraints_hash": report.constraints_hash,
        "solver": config.to_dict(),
        "status": report.status,
        "committed_ranges": [list(item) for item in report.committed_ranges],
    }
    lines = [canonical_json_bytes(header).decode("utf-8")]
    for result in sorted(report.results, key=lambda item: item.frame):
        lines.append(
            canonical_json_bytes(result.log_record()).decode("utf-8")
        )
    payload = ("\n".join(lines) + "\n").encode("utf-8")
    target = Path(path)
    _write_bytes(target, payload)
    return hash_text(payload.decode("utf-8"))


def _comparison(batch: RunReport, window: RunReport) -> dict[str, Any]:
    batch_by_frame = {item.frame: item for item in batch.results}
    window_by_frame = {item.frame: item for item in window.results}
    frames = sorted(set(batch_by_frame) | set(window_by_frame))
    max_residual_delta = 0.0
    max_pose_delta = 0.0
    hash_mismatches: list[int] = []
    missing_frames: list[int] = []
    for frame in frames:
        if frame not in batch_by_frame or frame not in window_by_frame:
            missing_frames.append(frame)
            continue
        left = batch_by_frame[frame]
        right = window_by_frame[frame]
        max_residual_delta = max(
            max_residual_delta, abs(left.total_residual - right.total_residual)
        )
        if left.pose_hash != right.pose_hash:
            hash_mismatches.append(frame)
            max_pose_delta = max(
                max_pose_delta,
                float(np.max(np.abs(left.pose.world - right.pose.world))),
            )
    expected = set(range(batch.frame_count))
    return {
        "batch_strategy": batch.strategy,
        "window_strategy": window.strategy,
        "frame_count": batch.frame_count,
        "compared_frames": len(frames),
        "complete": set(frames) == expected and batch.frame_count == window.frame_count,
        "missing_frames": missing_frames,
        "pose_hash_mismatches": hash_mismatches,
        "pose_hashes_equal": not hash_mismatches,
        "max_pose_abs_delta": max_pose_delta,
        "max_total_residual_delta": max_residual_delta,
        "declared_residual_tolerance": 1.0e-12,
        "declared_pose_tolerance": 1.0e-12,
        "within_tolerance": (
            not hash_mismatches
            and max_residual_delta <= 1.0e-12
            and max_pose_delta <= 1.0e-12
        ),
    }


def _summary(report: RunReport, trajectory_hash: str) -> dict[str, Any]:
    residuals = [item.total_residual for item in report.results]
    return {
        **report.summary(),
        "trajectory_sha256": trajectory_hash,
        "max_total_residual": max(residuals) if residuals else 0.0,
        "mean_total_residual": float(np.mean(residuals)) if residuals else 0.0,
        "conflict_frames": sorted(
            {int(item["frame"]) for item in report.conflict_reports}
        ),
        "pose_hash_first": report.results[0].pose_hash if report.results else None,
        "pose_hash_last": report.results[-1].pose_hash if report.results else None,
    }


def _run_sample_strategy(
    sample: Mapping[str, Any],
    out_root: Path,
    strategy: str,
    config: SolverConfig,
) -> tuple[RunReport, SolverConfig]:
    bvh_path = Path(sample["bvh"])
    constraints_path = Path(sample["constraints"])
    run_config = replace(config, strategy=strategy)
    run_dir = out_root / strategy
    report = SolvePipeline(
        bvh_path,
        constraints_path,
        run_dir / "snapshots",
        run_config,
    ).run(resume=False)
    trajectory_hash = write_trajectory(
        run_dir / "trajectory.jsonl",
        report,
        bvh_path=bvh_path,
        constraints_path=constraints_path,
        config=run_config,
    )
    write_json(run_dir / "summary.json", _summary(report, trajectory_hash))
    write_json(
        run_dir / "conflicts.json",
        {
            "schema": "mocap-conflicts-v1",
            "sample": sample["name"],
            "strategy": strategy,
            "conflicts": report.conflict_reports,
        },
    )
    return report, run_config


def _cache_experiment(sample: Mapping[str, Any]) -> dict[str, Any]:
    bvh_path = Path(sample["bvh"])
    constraints_path = Path(sample["constraints"])
    document = parse_bvh(bvh_path)
    constraints = load_constraints(constraints_path, document)
    config = SolverConfig(workers=1)
    cache = SolveCache(config.cache_max_entries)
    frames = list(range(document.frame_count))
    first = solve_frames(document, constraints, frames, config, cache=cache)
    target = constraints.constraints[0].id
    changed = constraints.with_parameter(target, tolerance=constraints.get(target).tolerance + 0.001)
    first_stats = cache.stats()
    second = solve_frames(document, changed, frames, config, cache=cache)
    second_stats = cache.stats()
    second_hits = second_stats["hits"] - first_stats["hits"]
    return {
        "sample": sample["name"],
        "frame_count": document.frame_count,
        "changed_constraint": target,
        "change": "tolerance-only",
        "poses_preserved": all(
            left.pose_hash == right.pose_hash for left, right in zip(first, second)
        ),
        "cache_hits_on_second_run": second_hits,
        "expected_hits": document.frame_count,
        "only_affected_or_reusable_frames_recomputed": second_hits == document.frame_count,
    }


def _determinism_check(sample: Mapping[str, Any], out_root: Path) -> dict[str, Any]:
    config = SolverConfig(strategy="window", workers=1)
    bvh_path = Path(sample["bvh"])
    constraints_path = Path(sample["constraints"])
    hashes: list[str] = []
    summaries: list[dict[str, Any]] = []
    for run_index in (1, 2):
        run_dir = out_root / f"run_{run_index}"
        report = SolvePipeline(
            bvh_path,
            constraints_path,
            run_dir / "snapshots",
            config,
        ).run(resume=False)
        trajectory_hash = write_trajectory(
            run_dir / "trajectory.jsonl",
            report,
            bvh_path=bvh_path,
            constraints_path=constraints_path,
            config=config,
        )
        hashes.append(trajectory_hash)
        summaries.append(_summary(report, trajectory_hash))
    first_bytes = (out_root / "run_1" / "trajectory.jsonl").read_bytes()
    second_bytes = (out_root / "run_2" / "trajectory.jsonl").read_bytes()
    return {
        "sample": sample["name"],
        "operation_sequence": ["solve", "write_trajectory"],
        "same_seed": Path(sample["baseline"]).exists(),
        "trajectory_bytes_equal": first_bytes == second_bytes,
        "trajectory_sha256": hashes,
        "summary_equal": summaries[0] == summaries[1],
    }


def load_sample_index(sample_dir: str | Path) -> dict[str, dict[str, Any]]:
    root = Path(sample_dir)
    index_path = root / "samples.json"
    if index_path.exists():
        raw = json.loads(index_path.read_text(encoding="utf-8"))
        out: dict[str, dict[str, Any]] = {}
        for item in raw.get("samples", []):
            name = str(item["name"])
            out[name] = {
                "name": name,
                "bvh": str(root / item["bvh"]),
                "constraints": str(root / item["constraints"]),
                "baseline": str(root / item["baseline"]),
            }
        return out
    out = {}
    for bvh_path in sorted(root.glob("*.bvh")):
        name = bvh_path.stem
        constraints = root / f"{name}.constraints.json"
        baseline = root / f"{name}.baseline.json"
        if constraints.exists():
            out[name] = {
                "name": name,
                "bvh": str(bvh_path),
                "constraints": str(constraints),
                "baseline": str(baseline),
            }
    return out


def run_experiment(
    sample_dir: str | Path,
    output_dir: str | Path,
    *,
    workers: int = 1,
) -> dict[str, Any]:
    samples = load_sample_index(sample_dir)
    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    required = ["small", "large", "conflict"]
    missing = [name for name in required if name not in samples]
    if missing:
        raise ValueError(f"missing required samples: {', '.join(missing)}")
    config = SolverConfig(workers=workers)
    experiment: dict[str, Any] = {
        "schema": "mocap-experiment-v1",
        "sample_dir": str(Path(sample_dir).resolve()),
        "solver": config.to_dict(),
        "scales": {},
        "comparisons": {},
        "conflicts": {},
        "cache": {},
        "determinism": {},
    }
    for name in ("small", "large"):
        sample = samples[name]
        sample_out = out / name
        wise: dict[str, Any] = {}
        reports: dict[str, RunReport] = {}
        for strategy in ("batch", "window"):
            report, _run_config = _run_sample_strategy(
                sample, sample_out, strategy, config
            )
            reports[strategy] = report
            wise[strategy] = _summary(
                report,
                hash_file(str(sample_out / strategy / "trajectory.jsonl")),
            )
        comparison = _comparison(reports["batch"], reports["window"])
        experiment["comparisons"][name] = comparison
        experiment["scales"][name] = wise
        write_json(sample_out / "comparison.json", comparison)
    conflict_sample = samples["conflict"]
    conflict_report, _ = _run_sample_strategy(
        conflict_sample, out / "conflict", "auto", config
    )
    experiment["conflicts"]["conflict"] = conflict_report.summary()
    if "unreachable" in samples:
        unreachable_report, _ = _run_sample_strategy(
            samples["unreachable"], out / "unreachable", "auto", config
        )
        experiment["conflicts"]["unreachable"] = unreachable_report.summary()
    experiment["cache"]["small"] = _cache_experiment(samples["small"])
    experiment["determinism"]["small"] = _determinism_check(
        samples["small"], out / "determinism_small"
    )
    write_json(out / "experiment.json", experiment)
    return experiment


if __name__ == "__main__":  # pragma: no cover
    import sys

    from .cli import main

    raise SystemExit(main(["experiment", *sys.argv[1:]]))

