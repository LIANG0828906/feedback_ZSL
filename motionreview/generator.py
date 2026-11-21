"""Deterministic local BVH sample and baseline generator."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Sequence

from .bvh import BVHDocument, evaluate_frame, parse_bvh
from .constraints import ConstraintSet, parse_constraints
from .hashing import canonical_json_bytes, sha256_bytes, sha256_json
from .solver import PoseSolver


@dataclass(frozen=True)
class SampleCase:
    name: str
    frames: int
    joints: int
    conflict: bool = False


DEFAULT_CASES: tuple[SampleCase, ...] = (
    SampleCase("short", 96, 7),
    SampleCase("medium", 240, 12),
    SampleCase("conflict", 64, 8, conflict=True),
    SampleCase("long", 1024, 16),
)


def _pretty_bvh(lines: Iterable[str]) -> str:
    return "\n".join(lines) + "\n"


def build_skeleton(case: SampleCase) -> tuple[list[dict[str, Any]], str]:
    names = ["Hips"]
    for index in range(1, case.joints):
        if index % 5 == 1:
            names.append(f"Spine{index:02d}")
        elif index % 5 == 2:
            names.append(f"Chest{index:02d}")
        elif index % 5 == 3:
            names.append(f"Neck{index:02d}")
        elif index % 5 == 4:
            names.append(f"Head{index:02d}")
        else:
            names.append(f"Arm{index:02d}")
    hierarchy: list[dict[str, Any]] = []
    for index, name in enumerate(names):
        offset = (
            (0.0, 0.0, 0.0)
            if index == 0
            else (
                2.5 if index % 4 == 0 else 0.0,
                3.0 + (index % 3),
                0.5 if index % 2 else 0.0,
            )
        )
        channels = (
            "Xposition",
            "Yposition",
            "Zposition",
            "Zrotation",
            "Xrotation",
            "Yrotation",
        ) if index == 0 else ("Zrotation", "Xrotation", "Yrotation")
        hierarchy.append(
            {
                "index": index,
                "name": name,
                "parent": None if index == 0 else index - 1,
                "offset": offset,
                "channels": channels,
            }
        )
    text_lines = ["HIERARCHY", f"ROOT {names[0]}", "{"]
    depth = 1
    for index, item in enumerate(hierarchy):
        if index > 0:
            prefix = " " * depth
            text_lines.append(f"{prefix}JOINT {item['name']}")
            text_lines.append(f"{prefix}{{")
            depth += 1
        local_indent = " " * depth
        offset = item["offset"]
        text_lines.append(
            f"{local_indent}OFFSET {offset[0]:.6f} {offset[1]:.6f} {offset[2]:.6f}"
        )
        channels = item["channels"]
        text_lines.append(
            f"{local_indent}CHANNELS {len(channels)} "
            + " ".join(channels)
        )
    end_prefix = " " * depth
    text_lines.extend(
        [
            f"{end_prefix}End Site",
            f"{end_prefix}{{",
            f"{end_prefix} OFFSET 0.000000 4.000000 0.000000",
            f"{end_prefix}}}",
        ]
    )
    for _ in range(case.joints):
        depth -= 1
        text_lines.append(" " * depth + "}")
    return hierarchy, _pretty_bvh(text_lines)


def _motion_row(
    index: int, channels: tuple[str, ...], case: SampleCase, seed: int
) -> list[float]:
    phase = (seed % 17) * 0.031 + index * 0.071
    values: list[float] = []
    for channel_index, channel in enumerate(channels):
        if channel == "Xposition":
            value = 0.15 * math.sin(phase)
        elif channel == "Yposition":
            value = 0.04 * math.cos(phase * 0.7)
        elif channel == "Zposition":
            value = 0.08 * math.sin(phase * 0.4)
        else:
            axis_phase = phase + 0.29 * (0.0 if channel[0] == "Z" else 1.0)
            # Declared limits are deliberately tight enough to require a
            # clamp on some frames but not so tight that constraints vanish.
            value = 0.55 * math.sin(axis_phase + channel_index * 0.13)
        values.append(value)
    return values


def case_bvh_text(case: SampleCase, *, seed: int = 42) -> str:
    hierarchy, text = build_skeleton(case)
    total_channels = sum(len(item["channels"]) for item in hierarchy)
    lines = [text.rstrip("\n"), "MOTION", f"Frames: {case.frames}", "Frame Time: 0.040000"]
    for frame in range(case.frames):
        flat: list[float] = []
        for item in hierarchy:
            flat.extend(
                _motion_row(
                    frame + int(item["index"]) * 3,
                    tuple(item["channels"]),
                    case,
                    seed,
                )
            )
        assert len(flat) == total_channels
        lines.append(" ".join(f"{value:.9f}" for value in flat))
    return _pretty_bvh(lines)


def _document(case: SampleCase, seed: int) -> BVHDocument:
    return parse_bvh(
        case_bvh_text(case, seed=seed), clip_id=case.name
    )


def case_constraints(
    document: BVHDocument, case: SampleCase
) -> ConstraintSet:
    constraints: list[dict[str, Any]] = []
    for index in range(1, min(len(document.joints), 6)):
        constraints.append(
            {
                "id": f"bone_{index}",
                "kind": "bone_length",
                "joint": document.joints[index].name,
                "weight": 1.0,
                "tolerance": 1e-6,
            }
        )
    for index in range(1, len(document.joints)):
        for axis in ("Z", "X", "Y"):
            constraints.append(
                {
                    "id": f"limit_{index}_{axis}",
                    "kind": "angle_limit",
                    "joint": document.joints[index].name,
                    "axis": axis,
                    "min": -0.70,
                    "max": 0.70,
                    "weight": 1.0,
                    "tolerance": 1e-6,
                }
            )

    target_joint = document.joints[-1].name
    # Targets are generated from the unmodified reference pose at selected
    # frames, then shifted by a reachable offset. Per-frame target maps make
    # affected-frame range calculations unambiguous.
    baseline = evaluate_frame(document, 0)
    position = baseline[document.joint_index(target_joint), :3, 3]
    frame_point = [
        float(position[0] + 0.35),
        float(position[1] + 0.12),
        float(position[2] - 0.08),
    ]
    constraints.append(
        {
            "id": "effector_primary",
            "kind": "effector_position",
            "joint": target_joint,
            "target": frame_point,
            "frames": [0, max(1, document.frame_count // 3)],
            "weight": 2.0,
            "tolerance": 0.02,
        }
    )
    if document.frame_count > 8:
        midpoint = document.frame_count // 2
        constraints.append(
            {
                "id": "effector_snapshot",
                "kind": "effector_position",
                "joint": target_joint,
                "frames": [midpoint, midpoint + 1],
                "targets": {
                    str(midpoint): [
                        float(position[0] + 0.5),
                        float(position[1] + 0.16),
                        float(position[2]),
                    ]
                },
                "weight": 1.5,
                "tolerance": 0.025,
            }
        )
    if case.conflict:
        conflict_frame = min(document.frame_count - 1, max(3, document.frame_count // 3))
        constraints.append(
            {
                "id": "effector_impossible",
                "kind": "effector_position",
                "joint": target_joint,
                "frames": [conflict_frame, conflict_frame + 1],
                "targets": {
                    str(conflict_frame): [1000.0, 1000.0, 1000.0]
                },
                "weight": 3.0,
                "tolerance": 1e-4,
            }
        )
    return parse_constraints(document, constraints)


def _config_dict(
    *,
    per_task_memory_bytes: int,
    total_memory_bytes: int,
    parallelism: int = 2,
    max_iterations: int = 40,
) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "max_parallelism": parallelism,
        "per_task_memory_bytes": per_task_memory_bytes,
        "total_memory_bytes": total_memory_bytes,
        "max_iterations": max_iterations,
        "default_cache_depth": 64,
        "checkpoint_frames": 32,
        "degradation_sequence": [
            {
                "name": "shrink_iteration_buffer",
                "iteration_frames": 32,
            },
            {
                "name": "batch_frame_ranges",
                "batch_frames": 16,
            },
            {
                "name": "reduce_cache_depth",
                "cache_depth": 1,
            },
        ],
    }


def generate_case(
    outdir: Path,
    case: SampleCase,
    *,
    seed: int,
    write_baseline: bool = True,
    max_iterations: int = 40,
) -> dict[str, Any]:
    case_dir = outdir / case.name
    case_dir.mkdir(parents=True, exist_ok=True)
    bvh_text = case_bvh_text(case, seed=seed)
    bvh_path = case_dir / f"{case.name}.bvh"
    bvh_path.write_text(bvh_text, encoding="utf-8")
    document = _document(case, seed)
    constraints = case_constraints(document, case)
    constraints_path = case_dir / "constraints.json"
    constraints_payload = {
        "schema": "constraint-config-v1",
        "constraints": [
            _constraint_to_json(constraint)
            for constraint in constraints.constraints
        ],
    }
    constraints_path.write_text(
        json.dumps(
            constraints_payload,
            indent=2,
            ensure_ascii=False,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )

    baseline_records: list[dict[str, Any]] = []
    baseline_aggregate: str | None = None
    if write_baseline:
        solver = PoseSolver(
            document,
            constraints,
            max_iterations=max_iterations,
            cache_depth=max(1, document.frame_count),
        )
        for frame in range(document.frame_count):
            solution = solver.solve_frame(frame)
            baseline_records.append(
                {
                    "frame": frame,
                    "status": solution.status,
                    "iterations": solution.iterations,
                    "total_residual": solution.total_residual,
                    "residuals": [
                        {
                            "constraint_id": record["constraint_id"],
                            "residual": record["residual"],
                            "weighted_residual": record[
                                "weighted_residual"
                            ],
                            "satisfied": record["satisfied"],
                        }
                        for record in solution.residuals
                    ],
                    "pose_hash": solution.pose_hash,
                    "content_hash": solution.content_hash,
                }
            )
        baseline_path = case_dir / "baseline.jsonl"
        data = "\n".join(
            canonical_json_bytes(record).decode("utf-8")
            for record in baseline_records
        ) + "\n"
        baseline_path.write_bytes(data.encode("utf-8"))
        baseline_aggregate = sha256_bytes(data.encode("utf-8"))

    manifest = {
        "schema": "sample-case-v1",
        "name": case.name,
        "seed": seed,
        "frames": case.frames,
        "joints": case.joints,
        "channels": document.channel_count,
        "conflict": case.conflict,
        "bvh": str(bvh_path.relative_to(outdir)),
        "constraints": str(constraints_path.relative_to(outdir)),
        "bvh_hash": document.content_hash,
        "constraint_version": constraints.parameter_version,
        "baseline_file": (
            str((case_dir / "baseline.jsonl").relative_to(outdir))
            if write_baseline
            else None
        ),
        "baseline_aggregate_hash": baseline_aggregate,
        "baseline_frames": len(baseline_records),
        "conflict_frames": sorted(
            {
                int(record["frame"])
                for record in baseline_records
                if record["status"] == "CONFLICT"
            }
        ),
    }
    return manifest


def _constraint_to_json(constraint: Any) -> dict[str, Any]:
    result: dict[str, Any] = {
        "id": constraint.id,
        "kind": constraint.kind,
        "joint": constraint.joint,
        "weight": constraint.weight,
        "tolerance": constraint.tolerance,
        "frames": [constraint.frame_start, constraint.frame_end],
    }
    if constraint.kind == "angle_limit":
        result.update(
            {
                "axis": constraint.axis,
                "min": constraint.minimum,
                "max": constraint.maximum,
            }
        )
    elif constraint.kind == "effector_position":
        if constraint.target is not None:
            result["target"] = list(constraint.target)
        if constraint.targets:
            result["targets"] = {
                str(frame): list(target)
                for frame, target in sorted(constraint.targets.items())
            }
    elif constraint.kind == "bone_length":
        result["length"] = constraint.rest_length
    return result


def generate_dataset(
    outdir: str | Path,
    *,
    cases: Sequence[SampleCase] = DEFAULT_CASES,
    seed: int = 42,
    write_baseline: bool = True,
    max_iterations: int = 40,
) -> dict[str, Any]:
    root = Path(outdir)
    root.mkdir(parents=True, exist_ok=True)
    manifests = [
        generate_case(
            root,
            case,
            seed=seed,
            write_baseline=write_baseline,
            max_iterations=max_iterations,
        )
        for case in cases
    ]
    config = _config_dict(
        per_task_memory_bytes=32 * 1024 * 1024,
        total_memory_bytes=64 * 1024 * 1024,
        parallelism=2,
        max_iterations=max_iterations,
    )
    tight_config = _config_dict(
        per_task_memory_bytes=2 * 1024 * 1024,
        total_memory_bytes=4 * 1024 * 1024,
        parallelism=2,
        max_iterations=max_iterations,
    )
    (root / "run_config.json").write_text(
        json.dumps(config, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    (root / "run_config_tight.json").write_text(
        json.dumps(tight_config, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    dataset_manifest = {
        "schema": "sample-dataset-v1",
        "seed": seed,
        "cases": manifests,
        "run_config": "run_config.json",
        "tight_run_config": "run_config_tight.json",
        "aggregate_hash": sha256_json(manifests),
    }
    (root / "manifest.json").write_text(
        json.dumps(
            dataset_manifest,
            indent=2,
            ensure_ascii=False,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    return dataset_manifest


def load_case_constraints(
    document: BVHDocument, path: str | Path
) -> ConstraintSet:
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    return parse_constraints(document, data)
