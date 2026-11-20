"""Deterministic local BVH and constraint sample generator."""

from __future__ import annotations

import json
import math
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .bvh import parse_bvh_text
from .errors import E_CONFIG_INVALID, BusinessError
from .math3d import hash_text


@dataclass(frozen=True)
class SampleSpec:
    name: str = "sample"
    seed: int = 20261121
    frames: int = 120
    joints: int = 16
    missing_rate: float = 0.08
    conflict: str = "none"

    def validate(self) -> None:
        if self.frames < 1:
            raise BusinessError(E_CONFIG_INVALID, "sample frame count must be positive")
        if self.joints < 6 or self.joints > 512:
            raise BusinessError(
                E_CONFIG_INVALID,
                "sample joint count must be between 6 and 512",
            )
        if not 0.0 <= self.missing_rate <= 1.0:
            raise BusinessError(
                E_CONFIG_INVALID,
                "missing_rate must be between 0 and 1",
            )
        if self.conflict not in {"none", "unreachable", "bone_length"}:
            raise BusinessError(
                E_CONFIG_INVALID,
                "conflict must be none, unreachable or bone_length",
            )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class _Node:
    name: str
    parent: str | None
    offset: tuple[float, float, float]


_BASE_NODES = [
    ("Hips", None, (0.0, 0.0, 0.0)),
    ("Spine", "Hips", (0.0, 0.25, 0.0)),
    ("Neck", "Spine", (0.0, 0.30, 0.0)),
    ("Head", "Neck", (0.0, 0.18, 0.0)),
    ("LeftShoulder", "Spine", (0.12, 0.24, 0.0)),
    ("LeftArm", "LeftShoulder", (0.24, 0.0, 0.0)),
    ("LeftHand", "LeftArm", (0.26, 0.0, 0.0)),
    ("RightShoulder", "Spine", (-0.12, 0.24, 0.0)),
    ("RightArm", "RightShoulder", (-0.24, 0.0, 0.0)),
    ("RightHand", "RightArm", (-0.26, 0.0, 0.0)),
    ("LeftUpLeg", "Hips", (0.11, -0.08, 0.0)),
    ("LeftLeg", "LeftUpLeg", (0.0, -0.42, 0.0)),
    ("LeftFoot", "LeftLeg", (0.0, -0.40, 0.0)),
    ("RightUpLeg", "Hips", (-0.11, -0.08, 0.0)),
    ("RightLeg", "RightUpLeg", (0.0, -0.42, 0.0)),
    ("RightFoot", "RightLeg", (0.0, -0.40, 0.0)),
]


def _build_nodes(joint_count: int) -> list[_Node]:
    selected = _BASE_NODES[: min(joint_count, len(_BASE_NODES))]
    nodes = [_Node(*item) for item in selected]
    names = {node.name for node in nodes}
    parent = "Spine" if "Spine" in names else "Hips"
    aux_index = 0
    while len(nodes) < joint_count:
        name = f"Aux_{aux_index:02d}"
        aux_index += 1
        offset = (0.07, 0.04 * ((aux_index % 3) - 1), 0.015 * (aux_index % 2))
        nodes.append(_Node(name, parent, offset))
        names.add(name)
        parent = name
    return nodes


def _end_offsets(nodes: list[_Node]) -> dict[str, tuple[float, float, float]]:
    children = {node.parent for node in nodes if node.parent is not None}
    offsets: dict[str, tuple[float, float, float]] = {}
    for node in nodes:
        if node.name in children:
            continue
        if "Hand" in node.name:
            offsets[node.name] = (0.09, 0.0, 0.0)
        elif "Foot" in node.name:
            offsets[node.name] = (0.08, 0.0, 0.12)
        elif node.name == "Head":
            offsets[node.name] = (0.0, 0.12, 0.0)
        elif node.name.startswith("Aux_"):
            offsets[node.name] = (0.07, 0.0, 0.0)
        else:
            offsets[node.name] = (0.0, 0.08, 0.0)
    return offsets


def _bvh_text(nodes: list[_Node], frames: int, frame_time: float = 1.0 / 30.0) -> str:
    children: dict[str | None, list[_Node]] = {}
    for node in nodes:
        children.setdefault(node.parent, []).append(node)
    ends = _end_offsets(nodes)

    lines: list[str] = ["HIERARCHY"]

    def emit(node: _Node, depth: int) -> None:
        indent = " " * depth
        keyword = "ROOT" if node.parent is None else "JOINT"
        lines.append(f"{indent}{keyword} {node.name}")
        lines.append(f"{indent}{{")
        lines.append(
            f"{indent} OFFSET {node.offset[0]:.9f} {node.offset[1]:.9f} {node.offset[2]:.9f}"
        )
        if node.parent is None:
            lines.append(
                f"{indent} CHANNELS 6 Zrotation Xrotation Yrotation "
                "Zposition Xposition Yposition"
            )
        else:
            lines.append(f"{indent} CHANNELS 3 Zrotation Xrotation Yrotation")
        for child in children.get(node.name, []):
            emit(child, depth + 1)
        if node.name in ends:
            offset = ends[node.name]
            lines.append(f"{indent} End Site")
            lines.append(f"{indent} {{")
            lines.append(
                f"{indent}  OFFSET {offset[0]:.9f} {offset[1]:.9f} {offset[2]:.9f}"
            )
            lines.append(f"{indent} }}")
        lines.append(f"{indent}}}")

    roots = children.get(None, [])
    if len(roots) != 1:
        raise AssertionError("sample skeleton must have one root")
    emit(roots[0], 0)
    lines.append("MOTION")
    lines.append(f"Frames: {frames}")
    lines.append(f"Frame Time: {frame_time:.9f}")
    return "\n".join(lines) + "\n"


def _rotation_track(
    nodes: list[_Node], frames: int, seed: int
) -> dict[str, np.ndarray]:
    indexes = {node.name: index for index, node in enumerate(nodes)}
    out: dict[str, np.ndarray] = {}
    for node in nodes:
        idx = indexes[node.name]
        if node.parent is None:
            amps = np.array([0.08, 0.05, 0.06])
            freqs = np.array([0.011, 0.007, 0.009])
        else:
            amps = np.array([0.16, 0.12, 0.18]) * (0.8 + 0.1 * (idx % 4))
            freqs = np.array([0.013 + 0.001 * (idx % 5), 0.009, 0.011 + 0.001 * (idx % 3)])
        phases = np.array(
            [
                ((seed + idx * 37 + axis * 101) % 997) / 997.0 * 2.0 * math.pi
                for axis in range(3)
            ]
        )
        frame_axis = np.arange(frames, dtype=np.float64)[:, None]
        track = amps[None, :] * np.sin(frame_axis * freqs[None, :] + phases[None, :])
        out[node.name] = track
    return out


def _motion_rows(nodes: list[_Node], frames: int, seed: int) -> list[str]:
    tracks = _rotation_track(nodes, frames, seed)
    rows: list[str] = []
    for frame in range(frames):
        values: list[float] = []
        for node in nodes:
            rz, rx, ry = tracks[node.name][frame]
            values.extend([rz, rx, ry])
            if node.parent is None:
                x = 0.012 * math.sin(frame * 0.017 + 0.3)
                y = 0.012 * math.sin(frame * 0.013 + 1.1)
                z = 0.008 * math.sin(frame * 0.019 + 2.0)
                values.extend([z, x, y])
        rows.append(" ".join(f"{value:.9f}" for value in values))
    return rows


def _constraints(
    document,
    spec: SampleSpec,
    nodes: list[_Node],
    ends: dict[str, tuple[float, float, float]],
) -> dict[str, Any]:
    rng = np.random.default_rng(spec.seed ^ 0x5EED)
    names = [node.name for node in nodes]
    constraints: list[dict[str, Any]] = []

    bone_candidates = [
        name for name in ("LeftArm", "RightArm", "LeftLeg", "RightLeg", "Spine")
        if name in names
    ]
    for joint_name in bone_candidates[:3]:
        joint_index = document.joint_names.index(joint_name)
        joint = document.joints[joint_index]
        target = float(np.linalg.norm(joint.offset))
        if spec.conflict == "bone_length" and joint_name == bone_candidates[0]:
            target += 0.075
        constraints.append(
            {
                "id": f"bone:{joint_name}",
                "type": "bone_length",
                "joint": joint_name,
                "target": round(target, 9),
                "weight": 1.0,
                "tolerance": 0.002,
            }
        )

    limit_candidates = [
        name
        for name in (
            "LeftArm",
            "RightArm",
            "LeftLeg",
            "RightLeg",
            "Neck",
            "Aux_00",
        )
        if name in names
    ]
    for joint_name in limit_candidates[:4]:
        for axis in ("z",):
            constraints.append(
                {
                    "id": f"limit:{joint_name}:{axis}",
                    "type": "joint_limit",
                    "joint": joint_name,
                    "axis": axis,
                    "min_deg": -55.0,
                    "max_deg": 55.0,
                    "weight": 1.0,
                    "tolerance_deg": 0.25,
                }
            )

    end_joint_names = [f"{leaf}_EndSite" for leaf in ends]
    preferred = [
        name
        for name in (
            "LeftHand_EndSite",
            "RightHand_EndSite",
            "Head_EndSite",
            "LeftFoot_EndSite",
            "RightFoot_EndSite",
            "Aux_00_EndSite",
        )
        if name in end_joint_names
    ]
    selected_effectors = preferred[:3]
    if len(selected_effectors) < 2:
        selected_effectors = end_joint_names[: min(3, len(end_joint_names))]
    missing_rng = np.random.default_rng(spec.seed ^ 0xBEEF)
    conflict_start = int(spec.frames * 0.66)
    for effector_rank, end_name in enumerate(selected_effectors):
        targets: list[list[float] | None] = []
        for frame in range(spec.frames):
            missing = bool(missing_rng.random() < spec.missing_rate)
            if missing:
                targets.append(None)
                continue
            pose = document.evaluate(frame)
            joint_index = document.joint_names.index(end_name)
            base = pose.joint_positions[joint_index]
            if spec.conflict == "unreachable" and frame >= conflict_start and effector_rank == 0:
                offset = np.array([3.2, 2.6, 1.8])
                target = base + offset
            else:
                parent_index = document.joints[joint_index].parent
                pivot = pose.joint_positions[parent_index]
                radial = base - pivot
                radius = max(float(np.linalg.norm(radial)), 1.0e-12)
                radial_unit = radial / radius
                direction = rng.normal(size=3)
                direction -= np.dot(direction, radial_unit) * radial_unit
                norm = max(float(np.linalg.norm(direction)), 1.0e-12)
                direction /= norm
                magnitude = 0.045 + 0.035 * float(rng.random())
                # Keep the target exactly on the reachable sphere of the
                # immediate parent.  A single unconstrained wrist/ankle
                # rotation can therefore satisfy it.
                candidate = base + direction * magnitude
                target = pivot + (candidate - pivot) / max(
                    float(np.linalg.norm(candidate - pivot)), 1.0e-12
                ) * radius
            targets.append([round(float(x), 9) for x in target])
        constraints.append(
            {
                "id": f"effector:{end_name}",
                "type": "effector",
                "joint": end_name,
                "targets": targets,
                "weight": 1.0,
                "tolerance": 0.008,
            }
        )
    return {
        "schema": "mocap-constraints-v1",
        "name": f"{spec.name}-constraints",
        "revision": 1,
        "seed": spec.seed,
        "constraints": constraints,
    }


def generate_sample(
    output_dir: str | Path,
    spec: SampleSpec,
) -> dict[str, Any]:
    spec.validate()
    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    nodes = _build_nodes(spec.joints)
    ends = _end_offsets(nodes)
    bvh_text = _bvh_text(nodes, spec.frames)
    bvh_text += "\n".join(_motion_rows(nodes, spec.frames, spec.seed)) + "\n"
    document = parse_bvh_text(bvh_text, "<generated>")
    constraints = _constraints(document, spec, nodes, ends)
    bvh_path = out / f"{spec.name}.bvh"
    constraints_path = out / f"{spec.name}.constraints.json"
    baseline_path = out / f"{spec.name}.baseline.json"
    bvh_path.write_text(bvh_text, encoding="utf-8", newline="\n")
    constraints_text = json.dumps(
        constraints,
        sort_keys=True,
        indent=2,
        ensure_ascii=False,
        allow_nan=False,
    ) + "\n"
    constraints_path.write_text(constraints_text, encoding="utf-8", newline="\n")
    pose_hashes = [document.frame_hash(frame) for frame in range(document.frame_count)]
    baseline = {
        "schema": "mocap-sample-baseline-v1",
        "spec": spec.to_dict(),
        "bvh_sha256": hash_text(bvh_text),
        "constraints_sha256": hash_text(constraints_text),
        "skeleton_fingerprint": document.skeleton_fingerprint(),
        "frame_count": document.frame_count,
        "joint_count_named": spec.joints,
        "joint_count_including_end_sites": document.joint_count,
        "pose_hashes": pose_hashes,
        "first_pose_hash": pose_hashes[0],
        "last_pose_hash": pose_hashes[-1],
        "missing_rate": spec.missing_rate,
        "conflict": spec.conflict,
    }
    baseline_path.write_text(
        json.dumps(
            baseline,
            sort_keys=True,
            indent=2,
            ensure_ascii=False,
            allow_nan=False,
        )
        + "\n",
        encoding="utf-8",
        newline="\n",
    )
    return {
        "name": spec.name,
        "bvh": str(bvh_path),
        "constraints": str(constraints_path),
        "baseline": str(baseline_path),
        "spec": spec.to_dict(),
        "baseline_data": baseline,
    }


PRESETS: dict[str, SampleSpec] = {
    "small": SampleSpec(
        name="small",
        seed=20261121,
        frames=90,
        joints=16,
        missing_rate=0.08,
        conflict="none",
    ),
    "large": SampleSpec(
        name="large",
        seed=20261122,
        frames=1200,
        joints=26,
        missing_rate=0.18,
        conflict="none",
    ),
    "conflict": SampleSpec(
        name="conflict",
        seed=20261123,
        frames=120,
        joints=20,
        missing_rate=0.10,
        conflict="bone_length",
    ),
    "unreachable": SampleSpec(
        name="unreachable",
        seed=20261124,
        frames=120,
        joints=20,
        missing_rate=0.10,
        conflict="unreachable",
    ),
}


def generate_presets(output_dir: str | Path) -> list[dict[str, Any]]:
    out = Path(output_dir)
    records = [generate_sample(out, PRESETS[key]) for key in ("small", "large", "conflict", "unreachable")]
    index = {
        "schema": "mocap-sample-index-v1",
        "samples": [
            {
                "name": record["name"],
                "bvh": Path(record["bvh"]).name,
                "constraints": Path(record["constraints"]).name,
                "baseline": Path(record["baseline"]).name,
                "spec": record["spec"],
            }
            for record in records
        ],
    }
    (out / "samples.json").write_text(
        json.dumps(index, sort_keys=True, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
        newline="\n",
    )
    return records


if __name__ == "__main__":  # pragma: no cover
    import sys

    from .cli import main

    raise SystemExit(main(["generate", *sys.argv[1:]]))

