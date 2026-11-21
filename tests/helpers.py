from __future__ import annotations

from pathlib import Path
from typing import Any

from motionreview.bvh import BVHDocument, evaluate_frame, parse_bvh
from motionreview.config import RunConfig
from motionreview.constraints import ConstraintSet, parse_constraints
from motionreview.generator import SampleCase, case_bvh_text


def sample_document(
    clip_id: str = "clip",
    *,
    frames: int = 24,
    joints: int = 5,
    conflict: bool = False,
    seed: int = 42,
) -> BVHDocument:
    case = SampleCase("sample", frames, joints, conflict)
    return parse_bvh(
        case_bvh_text(case, seed=seed), clip_id=clip_id
    )


def angle_only_constraints(
    document: BVHDocument, *, weight: float = 1.0
) -> ConstraintSet:
    raw: list[dict[str, Any]] = []
    for index in range(1, len(document.joints)):
        raw.append(
            {
                "id": f"limit_{index}",
                "kind": "angle_limit",
                "joint": document.joints[index].name,
                "axis": "Z",
                "min": -0.35,
                "max": 0.35,
                "weight": weight,
                "tolerance": 1e-7,
            }
        )
    return parse_constraints(document, raw)


def full_constraints(
    document: BVHDocument, *, conflict: bool = False
) -> ConstraintSet:
    raw: list[dict[str, Any]] = []
    for index in range(1, len(document.joints)):
        for axis in ("Z", "X", "Y"):
            raw.append(
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
    midpoint = document.frame_count // 2
    reference = evaluate_frame(document, 0)[
        document.joint_index(document.joints[-1].name), :3, 3
    ]
    early_target = [
        float(reference[0] + 0.35),
        float(reference[1] + 0.12),
        float(reference[2] - 0.08),
    ]
    midpoint_target = [
        float(reference[0] + 0.5),
        float(reference[1] + 0.16),
        float(reference[2]),
    ]
    raw.append(
        {
            "id": "effector_early",
            "kind": "effector_position",
            "joint": document.joints[-1].name,
            "frames": [0, max(1, document.frame_count // 4)],
            "target": early_target,
            "weight": 1.5,
            "tolerance": 0.02,
        }
    )
    raw.append(
        {
            "id": "effector_mid",
            "kind": "effector_position",
            "joint": document.joints[-1].name,
            "frames": [midpoint, midpoint + 1],
            "targets": {str(midpoint): midpoint_target},
            "weight": 1.5,
            "tolerance": 0.03,
        }
    )
    if conflict:
        raw.append(
            {
                "id": "effector_impossible",
                "kind": "effector_position",
                "joint": document.joints[-1].name,
                "frames": [midpoint, midpoint + 1],
                "targets": {
                    str(midpoint): [1000.0, 1000.0, 1000.0]
                },
                "weight": 2.0,
                "tolerance": 1e-4,
            }
        )
    return parse_constraints(document, raw)


def run_config(
    *,
    parallelism: int = 2,
    per_task: int = 8 * 1024 * 1024,
    total: int | None = None,
    max_iterations: int = 30,
    checkpoint: int = 8,
    cache_depth: int = 64,
) -> RunConfig:
    return RunConfig(
        max_parallelism=parallelism,
        per_task_memory_bytes=per_task,
        total_memory_bytes=(
            total if total is not None else per_task * 2
        ),
        max_iterations=max_iterations,
        checkpoint_frames=checkpoint,
        default_cache_depth=cache_depth,
    )


def committed_frame_hashes(root: Path, clip_id: str) -> dict[int, str]:
    from motionreview.storage import SnapshotStore

    state = SnapshotStore(root, clip_id).recover()
    return {solution.frame: solution.content_hash for solution in state.solutions}
