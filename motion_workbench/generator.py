from __future__ import annotations

import argparse
import json
import math
import os
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .bvh import BVHData, parse_bvh_text
from .hashing import sha256_bytes, sha256_text
from .session import WorkbenchSession
from .solver import residual_digest


@dataclass(frozen=True)
class SampleSpec:
    frames: int = 48
    joints: int = 12
    conflict: str = "none"
    seed: int = 7

    @property
    def stem(self) -> str:
        return (
            f"frames{self.frames:03d}_joints{self.joints:02d}_"
            f"{self.conflict}_seed{self.seed}"
        )


def _joint_names(count: int) -> list[str]:
    names = ["root"]
    for index in range(1, count):
        names.append("effector" if index == count - 1 else f"joint_{index:02d}")
    return names


def _offsets(count: int, seed: int) -> list[tuple[float, float, float]]:
    offsets: list[tuple[float, float, float]] = [(0.0, 0.0, 0.0)]
    for index in range(1, count):
        length = 0.28 + 0.035 * ((index + seed) % 4)
        angle = 0.47 * index + 0.11 * seed
        x = length * 0.42 * math.sin(angle)
        y = length * 0.72
        z = length * 0.42 * math.cos(angle)
        offsets.append((round(x, 6), round(y, 6), round(z, 6)))
    return offsets


def _motion_value(
    frame: int, joint: int, axis: int, seed: int
) -> float:
    phase = 0.37 * joint + 0.19 * axis + 0.07 * seed
    period = 37 + (joint % 5) * 4
    value = (
        13.0 * math.sin(2.0 * math.pi * frame / period + phase)
        + 4.5 * math.cos(2.0 * math.pi * frame / (period * 1.7) - phase)
        + 0.6 * math.sin(frame * 0.19 + joint)
    )
    return round(value, 6)


def generate_bvh_text(spec: SampleSpec) -> str:
    if spec.frames < 1:
        raise ValueError("frames must be at least one")
    if spec.joints < 2:
        raise ValueError("joints must be at least two")
    if spec.conflict not in {"none", "ee_unreachable", "joint_limit"}:
        raise ValueError(f"Unknown conflict mode: {spec.conflict}")
    names = _joint_names(spec.joints)
    offsets = _offsets(spec.joints, spec.seed)
    lines: list[str] = ["HIERARCHY"]
    for index, name in enumerate(names):
        indent = "  " * index
        if index == 0:
            lines.append("ROOT root")
        else:
            lines.append(f"{indent}JOINT {name}")
        lines.append(f"{indent}{{")
        ox, oy, oz = offsets[index]
        lines.append(f"{indent}  OFFSET {ox:.6f} {oy:.6f} {oz:.6f}")
        lines.append(
            f"{indent}  CHANNELS 3 Zrotation Xrotation Yrotation"
        )
        if index == spec.joints - 1:
            lines.append(f"{indent}  End Site")
            lines.append(f"{indent}  {{")
            lines.append(f"{indent}    OFFSET 0.000000 0.090000 0.000000")
            lines.append(f"{indent}  }}")
    for index in range(spec.joints - 1, -1, -1):
        indent = "  " * index
        lines.append(f"{indent}}}")
    lines.append("MOTION")
    lines.append(f"Frames: {spec.frames}")
    lines.append("Frame Time: 0.0333333")
    for frame in range(spec.frames):
        values: list[str] = []
        for joint in range(spec.joints):
            for axis in range(3):
                values.append(
                    f"{_motion_value(frame, joint, axis, spec.seed):.6f}"
                )
        lines.append(" ".join(values))
    return "\n".join(lines) + "\n"


def generate_constraints_dict(
    bvh: BVHData, spec: SampleSpec
) -> dict[str, Any]:
    constraints: list[dict[str, Any]] = []
    for index, joint in enumerate(bvh.joints):
        if index == 0:
            continue
        offset_length = math.sqrt(
            joint.offset[0] ** 2
            + joint.offset[1] ** 2
            + joint.offset[2] ** 2
        )
        constraints.append(
            {
                "id": f"bone:{joint.name}",
                "type": "bone_length",
                "joint": joint.name,
                "target_length": round(offset_length, 6),
                "weight": 1.0,
                "tolerance": 0.0005,
            }
        )
    for index, joint in enumerate(bvh.joints):
        if spec.conflict == "joint_limit":
            minimum = [0.0, 0.0, 0.0]
            maximum = [0.0, 0.0, 0.0]
            tolerance = 0.0001
            weight = 1.0
        else:
            minimum = [-24.0, -24.0, -24.0]
            maximum = [24.0, 24.0, 24.0]
            tolerance = 0.05
            weight = 0.6
        constraints.append(
            {
                "id": f"limit:{joint.name}",
                "type": "joint_limit",
                "joint": joint.name,
                "min_deg": minimum,
                "max_deg": maximum,
                "weight": weight,
                "tolerance": tolerance,
            }
        )

    target_joint_index = bvh.joint_by_name["effector"]
    targets: list[list[Any]] = []
    for frame in range(bvh.frame_count):
        transform = bvh.evaluate(frame)
        base = transform.world_positions[target_joint_index]
        if spec.conflict == "ee_unreachable":
            target = (
                30.0 + 0.01 * frame,
                30.0 + 0.02 * frame,
                30.0,
            )
            tolerance = 0.0001
            weight = 1.5
        else:
            target = (
                base[0] + 0.010 * math.sin(frame * 0.31 + spec.seed),
                base[1] + 0.007 * math.cos(frame * 0.23 + spec.seed),
                base[2] + 0.008 * math.sin(frame * 0.17 + 0.5 * spec.seed),
            )
            tolerance = 0.008
            weight = 1.2
        targets.append([frame, [round(value, 6) for value in target]])
    constraints.append(
        {
            "id": "ee:effector",
            "type": "effector_target",
            "joint": "effector",
            "weight": weight,
            "tolerance": tolerance,
            "targets": targets,
        }
    )
    return {
        "schema": "motion-workbench/constraints/v1",
        "name": spec.stem,
        "solver": {
            "max_iterations": 36 if spec.conflict == "none" else 20,
            "rotation_step_deg": 5.0,
            "scale_step": 0.02,
            "min_rotation_step_deg": 0.01,
            "min_scale_step": 0.00001,
        },
        "constraints": constraints,
    }


def generate_sample(
    output_dir: str | Path,
    spec: SampleSpec,
    *,
    keep_baseline_session: bool = True,
) -> dict[str, Any]:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    stem = spec.stem
    bvh_path = output / f"{stem}.bvh"
    constraints_path = output / f"{stem}.constraints.json"
    baseline_path = output / f"{stem}.baseline.json"
    bvh_text = generate_bvh_text(spec)
    bvh = parse_bvh_text(bvh_text, source_hash=sha256_text(bvh_text))
    constraints = generate_constraints_dict(bvh, spec)
    bvh_path.write_text(bvh_text, encoding="utf-8", newline="\n")
    constraints_text = (
        json.dumps(constraints, ensure_ascii=False, indent=2, sort_keys=True)
        + "\n"
    )
    constraints_path.write_text(
        constraints_text, encoding="utf-8", newline="\n"
    )

    session_root: Path
    if keep_baseline_session:
        session_root = output / f"{stem}.session"
        if session_root.exists():
            shutil.rmtree(session_root)
        temporary = None
    else:
        temporary = tempfile.TemporaryDirectory(
            prefix=f".{stem}.baseline.", dir=str(output)
        )
        session_root = Path(temporary.name)
    try:
        session = WorkbenchSession(
            bvh_path, constraints_path, session_root
        )
        report = session.solve(run_id=f"baseline:{stem}", chunk_size=8)
        frames = report.frames
        frame_hashes = [
            {
                "frame": solution.frame,
                "content_hash": solution.frame_hash,
                "converged": solution.converged,
            }
            for solution in sorted(frames.values(), key=lambda item: item.frame)
        ]
        baseline = {
            "schema": "motion-workbench/baseline/v1",
            "sample": spec.stem,
            "frames": spec.frames,
            "joints": spec.joints,
            "conflict_mode": spec.conflict,
            "seed": spec.seed,
            "status": report.status,
            "bvh_sha256": sha256_bytes(bvh_text.encode("utf-8")),
            "constraints_sha256": sha256_text(constraints_text),
            "residual_digest": residual_digest(frames.values()),
            "frame_hashes": frame_hashes,
            "interval_content_hashes": [
                interval["content_hash"] for interval in report.intervals
            ],
            "conflict": report.conflict,
            "trace_path": (
                str((session_root / "trace.jsonl").relative_to(output))
                if keep_baseline_session
                else None
            ),
        }
        data = (
            json.dumps(baseline, ensure_ascii=False, indent=2, sort_keys=True)
            + "\n"
        )
        temporary_path = baseline_path.with_suffix(".json.tmp")
        temporary_path.write_text(data, encoding="utf-8", newline="\n")
        os.replace(temporary_path, baseline_path)
        return {
            "spec": spec,
            "bvh_path": bvh_path,
            "constraints_path": constraints_path,
            "baseline_path": baseline_path,
            "baseline": baseline,
            "session_dir": session_root,
        }
    finally:
        if temporary is not None:
            temporary.cleanup()


def generate_suite(
    output_dir: str | Path, *, seed: int = 7
) -> list[dict[str, Any]]:
    specs = [
        SampleSpec(frames=24, joints=8, conflict="none", seed=seed),
        SampleSpec(frames=48, joints=12, conflict="none", seed=seed + 1),
        SampleSpec(frames=64, joints=16, conflict="none", seed=seed + 2),
        SampleSpec(frames=24, joints=10, conflict="ee_unreachable", seed=seed + 3),
        SampleSpec(frames=24, joints=10, conflict="joint_limit", seed=seed + 4),
    ]
    return [
        generate_sample(output_dir, spec, keep_baseline_session=False)
        for spec in specs
    ]


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Generate deterministic local BVH/constraint samples"
    )
    parser.add_argument("--out", default="samples", help="Output directory")
    parser.add_argument("--frames", type=int, default=48)
    parser.add_argument("--joints", type=int, default=12)
    parser.add_argument(
        "--conflict",
        choices=["none", "ee_unreachable", "joint_limit"],
        default="none",
    )
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--suite", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.suite:
        results = generate_suite(args.out, seed=args.seed)
    else:
        results = [
            generate_sample(
                args.out,
                SampleSpec(
                    frames=args.frames,
                    joints=args.joints,
                    conflict=args.conflict,
                    seed=args.seed,
                ),
            )
        ]
    print(
        json.dumps(
            [
                {
                    "bvh": str(result["bvh_path"]),
                    "constraints": str(result["constraints_path"]),
                    "baseline": str(result["baseline_path"]),
                    "status": result["baseline"]["status"],
                    "residual_digest": result["baseline"]["residual_digest"],
                }
                for result in results
            ],
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
