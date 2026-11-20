"""重定向结果的一致性校验与偏差统计。

真值来源：同一动作源骨架的逐关节全局朝向轨迹（正向运动学得到）。
校验方式：

1. 用绑定对齐把源全局朝向映射为目标“期望全局朝向”
   ``expected = align_i * S_global_i * inv(align_parent)``；
2. 与重定向输出的目标全局朝向逐关节逐帧比对测地角（主指标）；
3. 同时把输出的目标局部姿态重新正向计算，检查 FK 自洽性；
4. 超差定位到帧号+关节；跨版本统计差量超差以
   :data:`~retarget_pipeline.errors.E_CONSISTENCY_FAIL` 报告版本对。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .errors import E_CONSISTENCY_FAIL, PipelineError
from .mapping import JointMapping, target_driver_index
from .math3d import (
    forward_kinematics,
    quat_angle,
    quat_identity,
    quat_inv,
    quat_mul,
)
from .model import AnimDocument, RetargetResult
from .retarget import compute_alignment, frame_global_poses


@dataclass
class DeviationCell:
    frame: int
    target_index: int
    target_name: str
    source_index: int
    source_name: str
    angle_rad: float


@dataclass
class ValidationReport:
    tolerance_rad: float
    per_joint: dict[str, dict[str, float]] = field(default_factory=dict)
    cells: list[DeviationCell] = field(default_factory=list)
    exceeded: list[dict] = field(default_factory=list)
    max_joint: str | None = None
    max_angle_rad: float = 0.0
    skipped_frames: list[int] = field(default_factory=list)
    fk_self_consistency_max_rad: float = 0.0

    @property
    def passed(self) -> bool:
        return not self.exceeded

    def to_dict(self) -> dict:
        return {
            "tolerance_rad": self.tolerance_rad,
            "passed": self.passed,
            "max_joint": self.max_joint,
            "max_angle_rad": self.max_angle_rad,
            "max_angle_deg": float(np.degrees(self.max_angle_rad)),
            "fk_self_consistency_max_deg": float(
                np.degrees(self.fk_self_consistency_max_rad)
            ),
            "exceeded": list(self.exceeded),
            "skipped_frames": list(self.skipped_frames),
            "per_joint": self.per_joint,
            "cells": [c.__dict__ for c in self.cells],
        }


def validate_retarget(
    source: AnimDocument,
    target: AnimDocument,
    mapping: JointMapping,
    result: RetargetResult,
    tolerance_rad: float,
) -> ValidationReport:
    """以源全局朝向轨迹为真值，校验重定向目标全局朝向偏差。"""

    driver = target_driver_index(mapping)
    alignment = compute_alignment(source, target, mapping)
    target_parents = target.parents
    report = ValidationReport(tolerance_rad=float(tolerance_rad))
    accum: dict[int, list[float]] = {}

    for frame_result in result.frames:
        frame = frame_result.frame
        if not frame_result.resolved:
            report.skipped_frames.append(frame)
            continue

        src_global, _ = frame_global_poses(source, frame)

        # 落盘局部姿态重新 FK，检查全局/局部自洽。
        zero_t = [np.zeros(3) for _ in target.joints]
        re_global, _ = forward_kinematics(
            target.parents,
            [frame_result.local_rotations[i] for i in range(target.joint_count)],
            zero_t,
            target.scales,
        )
        re_global = np.stack(re_global, axis=0)
        report.fk_self_consistency_max_rad = max(
            report.fk_self_consistency_max_rad,
            max(
                quat_angle(re_global[t], frame_result.global_rotations[t])
                for t in range(target.joint_count)
            ),
        )

        for t_idx, s_idx in driver.items():
            align_i = alignment[t_idx]
            parent_t = target_parents[t_idx]
            if parent_t is None or parent_t < 0 or parent_t not in alignment:
                parent_corr = quat_identity()
            else:
                parent_corr = alignment[parent_t]
            expected = quat_mul(
                quat_mul(align_i, src_global[s_idx]), quat_inv(parent_corr)
            )
            angle = float(quat_angle(expected, frame_result.global_rotations[t_idx]))
            report.cells.append(DeviationCell(
                frame=frame,
                target_index=t_idx,
                target_name=target.joints[t_idx].name,
                source_index=s_idx,
                source_name=source.joints[s_idx].name,
                angle_rad=angle,
            ))
            accum.setdefault(t_idx, []).append(angle)
            if angle > tolerance_rad:
                report.exceeded.append({
                    "frame": frame,
                    "target_index": t_idx,
                    "target_name": target.joints[t_idx].name,
                    "source_name": source.joints[s_idx].name,
                    "angle_rad": angle,
                    "angle_deg": float(np.degrees(angle)),
                })

    for t_idx, angles in accum.items():
        name = target.joints[t_idx].name
        report.per_joint[name] = {
            "mean_rad": float(np.mean(angles)),
            "max_rad": float(np.max(angles)),
            "rms_rad": float(np.sqrt(np.mean(np.square(angles)))),
            "samples": float(len(angles)),
        }

    if report.cells:
        worst = max(report.cells, key=lambda c: c.angle_rad)
        report.max_joint = worst.target_name
        report.max_angle_rad = worst.angle_rad
    return report


def compare_version_runs(
    reports: dict[str, ValidationReport],
    tolerance_rad: float,
) -> dict:
    """比较同一动作不同版本输入的最大偏差差量。

    所有版本对的最大偏差差量需落在 ``tolerance_rad`` 内，否则抛
    :data:`E_CONSISTENCY_FAIL` 并报告版本对。
    """

    versions = sorted(reports)
    max_angles = {v: reports[v].max_angle_rad for v in versions}
    failures: list[dict] = []
    for i in range(len(versions)):
        for j in range(i + 1, len(versions)):
            va, vb = versions[i], versions[j]
            delta = abs(max_angles[va] - max_angles[vb])
            if delta > tolerance_rad:
                failures.append({
                    "code": E_CONSISTENCY_FAIL,
                    "version_pair": [va, vb],
                    "max_angle_delta_rad": float(delta),
                    "max_angle_delta_deg": float(np.degrees(delta)),
                    "tolerance_rad": float(tolerance_rad),
                })
    summary = {
        "tolerance_rad": float(tolerance_rad),
        "max_angles_rad": max_angles,
        "max_angles_deg": {k: float(np.degrees(v)) for k, v in max_angles.items()},
        "failures": failures,
        "passed": not failures,
    }
    if failures:
        raise PipelineError(
            E_CONSISTENCY_FAIL,
            "跨版本重定向偏差统计超差: "
            + ", ".join(str(f["version_pair"]) for f in failures),
            failures,
        )
    return summary


def source_truth_trajectory(doc: AnimDocument) -> np.ndarray:
    """暴露源骨架逐帧全局朝向真值轨迹，形状 (F, N, 4)。"""

    out = []
    for frame in range(doc.frame_count):
        glob, _ = frame_global_poses(doc, frame)
        out.append(glob)
    return np.stack(out, axis=0)
