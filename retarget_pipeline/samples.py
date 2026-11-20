"""本地确定性样本生成器。

所有随机性来自固定种子的 :class:`numpy.random.RandomState`，相同种子与
操作序列下，导出动画与轨迹日志逐字节一致。生成内容覆盖：

* 多种关节数 / 帧数 / 版本组合；
* v3 别名表、骨骼长度缩放、绑定姿态参考矩阵；
* 近重合旋转（小于 :data:`~retarget_pipeline.math3d.NEAR_COINCIDENT_ANGLE`）；
* 同一动作的 v1/v2/v3 三版本（旋转轨迹同源，保证可比对）；
* 基线偏差摘要与内容哈希。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass

import numpy as np

from .codec import canonical_bytes, dump_file
from .math3d import NEAR_COINCIDENT_ANGLE, make_transform, quat_normalize
from .model import AnimDocument, JointDef
from .naming import normalize_name


# 固定骨架模板（名字刻意混用大小写/分隔符，考验规范化匹配）
SOURCE_TEMPLATE = [
    ("Hips", -1),
    ("Spine", 0),
    ("Spine1", 1),
    ("Neck", 2),
    ("Head", 3),
    ("L_Shoulder", 2),
    ("L_Arm", 5),
    ("L_Hand", 6),
    ("R_Shoulder", 2),
    ("R_Arm", 8),
    ("R_Hand", 9),
    ("L_UpLeg", 0),
    ("L_Leg", 11),
    ("L_Foot", 12),
    ("R_UpLeg", 0),
    ("R_Leg", 14),
    ("R_Foot", 15),
]

# 目标骨架：命名风格不同（连字符/点/缩写），通过别名与规范化名映射
TARGET_TEMPLATE = [
    ("pelvis", -1, [], ["hips"]),
    ("spine", 0, [], []),
    ("spine-1", 1, [], ["spine1"]),
    ("neck", 2, [], []),
    ("head", 3, [], []),
    ("left.shoulder", 2, [], ["l_shoulder"]),
    ("left-arm", 5, [], ["l_arm"]),
    ("left-hand", 6, [], ["l_hand"]),
    ("right.shoulder", 2, [], ["r_shoulder"]),
    ("right-arm", 8, [], ["r_arm"]),
    ("right-hand", 9, [], ["r_hand"]),
    ("left-up-leg", 0, [], ["l_upleg"]),
    ("left-leg", 11, [], ["l_leg"]),
    ("left-foot", 12, [], ["l_foot"]),
    ("right-up-leg", 0, [], ["r_upleg"]),
    ("right-leg", 14, [], ["r_leg"]),
    ("right-foot", 15, [], ["r_foot"]),
]


@dataclass
class SampleSet:
    name: str
    seed: int
    joint_count: int
    frame_count: int
    versions: tuple[int, ...]
    paths: dict[str, str]
    target_path: str
    has_near_coincident: bool


def _axis_angle_quat(rng: np.random.RandomState, max_angle: float) -> np.ndarray:
    axis = rng.normal(size=3)
    norm = np.linalg.norm(axis)
    axis = axis / norm if norm > 1e-12 else np.array([0.0, 1.0, 0.0])
    angle = float(rng.uniform(-max_angle, max_angle))
    half = angle * 0.5
    return quat_normalize([np.cos(half), *(axis * np.sin(half))])


def _build_rotations(seed: int, joint_count: int, frame_count: int,
                     near_coincident: bool) -> np.ndarray:
    rng = np.random.RandomState(seed)
    rotations = np.zeros((frame_count, joint_count, 4), dtype=np.float64)
    base = np.array(
        [_axis_angle_quat(rng, 0.6) for _ in range(joint_count)],
        dtype=np.float64,
    )
    for f in range(frame_count):
        for j in range(joint_count):
            delta = _axis_angle_quat(rng, 0.25)
            from .math3d import quat_mul

            rotations[f, j] = quat_normalize(quat_mul(base[j], delta))
    if near_coincident and frame_count >= 2:
        # 让相邻两帧在首个关节上仅差一个极小角度（近重合旋转）。
        tiny = NEAR_COINCIDENT_ANGLE * 0.4
        h = tiny * 0.5
        q_tiny = quat_normalize([np.cos(h), 0.0, 0.0, np.sin(h)])
        from .math3d import quat_mul

        rotations[1, 0] = quat_normalize(quat_mul(rotations[0, 0], q_tiny))
    return rotations


def _build_translations(seed: int, joint_count: int, frame_count: int) -> np.ndarray:
    rng = np.random.RandomState(seed + 7919)
    trans = np.zeros((frame_count, joint_count, 3), dtype=np.float64)
    # 非根关节的静态绑定偏移
    for j in range(1, joint_count):
        trans[:, j, :] = rng.normal(scale=0.5, size=3)
    # 根关节逐帧位移（行走轨迹）
    for f in range(frame_count):
        trans[f, 0, :] = [0.1 * f, 0.0, 0.05 * np.sin(f * 0.3)]
    return trans


def build_source_documents(seed: int, joint_count: int, frame_count: int,
                           versions: tuple[int, ...] = (1, 2, 3),
                           near_coincident: bool = True) -> dict[int, AnimDocument]:
    """构建同一动作的多版本源动画文档（旋转轨迹同源）。"""

    template = SOURCE_TEMPLATE[:joint_count]
    rotations = _build_rotations(seed, joint_count, frame_count, near_coincident)
    translations = _build_translations(seed, joint_count, frame_count)

    docs: dict[int, AnimDocument] = {}
    for version in versions:
        joints: list[JointDef] = []
        for idx, (name, parent) in enumerate(template):
            p = None if (version == 1 or parent < 0) else parent
            bind = None
            scale = 1.0
            aliases: list[str] = []
            if version >= 3:
                bind = make_transform(np.eye(3), translations[0, idx])
                from .math3d import quat_to_matrix

                bind[:3, :3] = quat_to_matrix(rotations[0, idx])
                # 确定性缩放：部分关节长度缩放，制造非单位缩放场景
                scale = float(1.0 + 0.25 * ((idx % 3) - 1))
                aliases = [normalize_name(name)]
            joints.append(JointDef(name=name, parent=p, bind_matrix=bind,
                                   scale=scale, aliases=aliases))

        doc_trans = translations if version >= 2 else np.zeros_like(translations)
        doc = AnimDocument(
            version=version,
            joints=joints,
            local_rotations=rotations.copy(),
            local_translations=doc_trans.copy(),
            frame_rate=30.0,
            metadata={"seed": str(seed), "generator": "deterministic-samples"},
        )
        docs[version] = doc
    return docs


def build_target_document(seed: int, joint_count: int) -> AnimDocument:
    """构建 v3 目标骨架（不同命名风格、自带别名与缩放）。"""
    template = TARGET_TEMPLATE[:joint_count]
    joints: list[JointDef] = []
    frame_count = 1
    translations = _build_translations(seed + 13, joint_count, frame_count)[0]
    rotations = _build_rotations(seed + 17, joint_count, 1, False)[0]
    for idx, (name, parent, _norm, aliases) in enumerate(template):
        bind = make_transform(np.eye(3), translations[idx])
        from .math3d import quat_to_matrix

        bind[:3, :3] = quat_to_matrix(rotations[idx])
        scale = float(1.0 + 0.2 * ((idx % 4) - 1.5))
        joints.append(JointDef(
            name=name,
            parent=None if parent < 0 else parent,
            bind_matrix=bind,
            scale=scale,
            aliases=list(aliases),
        ))
    # 单帧“静止绑定”目标文档：动画通道取绑定姿态局部旋转。
    rot_channels = rotations[None, :, :].copy()
    trans_channels = translations[None, :, :].copy()
    return AnimDocument(
        version=3,
        joints=joints,
        local_rotations=rot_channels,
        local_translations=trans_channels,
        frame_rate=30.0,
        metadata={"seed": str(seed), "role": "target"},
    )


def generate_sample_set(out_dir: str, name: str, seed: int, joint_count: int,
                        frame_count: int, versions: tuple[int, ...] = (1, 2, 3)
                        ) -> SampleSet:
    """生成并落盘一套样本，返回文件路径清单。"""

    set_dir = os.path.join(out_dir, name)
    os.makedirs(set_dir, exist_ok=True)
    docs = build_source_documents(seed, joint_count, frame_count, versions,
                                  near_coincident=True)
    target = build_target_document(seed, joint_count)

    paths: dict[str, str] = {}
    hashes: dict[str, str] = {}
    for version, doc in docs.items():
        rel = f"{name}_source_v{version}.json"
        path = os.path.join(set_dir, rel)
        dump_file(doc, path)
        paths[f"v{version}"] = rel
        hashes[f"v{version}"] = _hash_doc(doc)

    target_rel = f"{name}_target_v3.json"
    dump_file(target, os.path.join(set_dir, target_rel))
    hashes["target"] = _hash_doc(target)

    manifest = {
        "name": name,
        "seed": seed,
        "joint_count": joint_count,
        "frame_count": frame_count,
        "versions": list(versions),
        "has_near_coincident": True,
        "files": {**paths, "target": target_rel},
        "content_hashes": hashes,
        "baseline": {
            "note": "基线偏差由 scripts/run_demo.py 重定向后回填",
            "tolerance_rad": 1e-6,
        },
    }
    with open(os.path.join(set_dir, "manifest.json"), "w", encoding="utf-8") as handle:
        json.dump(manifest, handle, ensure_ascii=False, sort_keys=True, indent=2)
        handle.write("\n")

    return SampleSet(
        name=name, seed=seed, joint_count=joint_count, frame_count=frame_count,
        versions=versions, paths=paths, target_path=target_rel,
        has_near_coincident=True,
    )


def _hash_doc(doc: AnimDocument) -> str:
    import hashlib

    return hashlib.sha256(canonical_bytes(doc)).hexdigest()


def generate_default_library(out_dir: str) -> list[SampleSet]:
    """生成验收用默认样本库：小/中/大三档 × 三版本。"""

    specs = [
        ("small", 101, 6, 8),
        ("medium", 202, 12, 24),
        ("large", 303, 17, 48),
    ]
    sets: list[SampleSet] = []
    for name, seed, joints, frames in specs:
        sets.append(generate_sample_set(out_dir, name, seed, joints, frames))
    return sets
