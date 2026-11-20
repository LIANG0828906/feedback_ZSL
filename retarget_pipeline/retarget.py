"""绑定姿态对齐与动作重定向。

重定向在全局空间进行，保证与源文件层级表达方式（v1 线性链 / v2 父子表 /
v3 绑定矩阵）解耦，从而让同一动作的不同版本输入得到一致结果：

1. 对源、目标文档做正向运动学，得到逐关节全局绑定姿态与逐帧全局姿态；
2. 绑定姿态差 ``align_i = bindTgt_i * inv(bindSrc_i)``，把源动作空间
   对齐到目标绑定空间；
3. 目标全局朝向 ``T = align_i * S_i * inv(align_parent)`` 级联，
   再由目标层级反算局部朝向；
4. v3 骨骼长度缩放只烘焙进目标根位移一次（沿关节自身局部轴），
   旋转级联不带缩放，杜绝父子重复缩放。
"""

from __future__ import annotations

from collections.abc import Callable

import numpy as np

from .errors import E_CANCELLED, E_POSE_UNRESOLVED, PipelineError
from .mapping import JointMapping, target_driver_index
from .math3d import (
    forward_kinematics,
    matrix_to_quat,
    nearest_rotation_matrix,
    quat_identity,
    quat_inv,
    quat_mul,
    quat_normalize,
    quat_relative,
    quat_rotate,
    quat_to_matrix,
    transform_rotation,
)
from .model import AnimDocument, RetargetFrame, RetargetResult

CancelCheck = Callable[[], bool] | None


def global_bind_rotation(doc: AnimDocument) -> np.ndarray:
    """由 v3 绑定参考矩阵求每个关节的全局绑定朝向，形状 (N, 4)。"""

    local_rot = [matrix_to_quat(transform_rotation(j.bind_matrix)) for j in doc.joints]
    local_t = [j.bind_matrix[:3, 3] for j in doc.joints]
    global_rot, _ = forward_kinematics(doc.parents, local_rot, local_t, doc.scales)
    return np.stack(global_rot, axis=0)


def frame_global_poses(doc: AnimDocument, frame: int) -> tuple[np.ndarray, np.ndarray]:
    """单帧正向运动学，返回全局旋转 (N,4) 与全局位置 (N,3)。"""

    rot, pos = forward_kinematics(
        doc.parents,
        doc.frame_local_rotations(frame),
        doc.frame_local_translations(frame),
        doc.scales,
    )
    return np.stack(rot, axis=0), np.stack(pos, axis=0)


def compute_alignment(source: AnimDocument, target: AnimDocument,
                      mapping: JointMapping) -> dict[int, np.ndarray]:
    """计算每个被驱动目标关节的绑定姿态差旋转 ``bindTgt * inv(bindSrc)``。"""

    src_bind = global_bind_rotation(source)
    tgt_bind = global_bind_rotation(target)
    driver = target_driver_index(mapping)
    align: dict[int, np.ndarray] = {}
    for t_idx, s_idx in driver.items():
        align[t_idx] = quat_normalize(
            quat_mul(tgt_bind[t_idx], quat_inv(src_bind[s_idx]))
        )
    return align


def retarget_frames(
    source: AnimDocument,
    target: AnimDocument,
    mapping: JointMapping,
):
    """逐帧生成器：每 yield 一个 :class:`RetargetFrame`。

    引擎可在每帧之间检查取消信号并提交快照；单帧内部的数学求解是
    不可中断步骤，异常被捕获并转为 ``E_POSE_UNRESOLVED`` 帧。
    """

    driver = target_driver_index(mapping)
    alignment = compute_alignment(source, target, mapping)
    n_target = target.joint_count
    target_parents = target.parents
    bind_global = global_bind_rotation(target)

    # 源根驱动的目标根关节（用于根位移）；找不到则退化为目标索引 0。
    root_target = next(
        (t for t, s in driver.items()
         if s == 0 and (target_parents[t] is None or target_parents[t] < 0)),
        0,
    )

    for frame in range(source.frame_count):
        try:
            src_global, src_positions = frame_global_poses(source, frame)
            tgt_global = np.tile(quat_identity(), (n_target, 1))

            for t_idx in range(n_target):
                s_idx = driver.get(t_idx)
                if s_idx is None:
                    # 无任何驱动（含祖先回退缺失）：保持目标绑定朝向。
                    tgt_global[t_idx] = bind_global[t_idx]
                    continue
                align_i = alignment[t_idx]
                parent_t = target_parents[t_idx]
                if parent_t is None or parent_t < 0 or parent_t not in alignment:
                    parent_corr = quat_identity()
                else:
                    parent_corr = alignment[parent_t]
                # T_global = align_i * S_global_i * inv(align_parent)
                desired = quat_mul(quat_mul(align_i, src_global[s_idx]), quat_inv(parent_corr))
                # 数值投影到最近合法旋转；矩阵退化时 matrix_to_quat 抛错。
                tgt_global[t_idx] = matrix_to_quat(
                    nearest_rotation_matrix(quat_to_matrix(desired))
                )

            # 由全局朝向按目标层级反算局部朝向
            tgt_local = np.tile(quat_identity(), (n_target, 1))
            for t_idx in range(n_target):
                parent_t = target_parents[t_idx]
                if parent_t is None or parent_t < 0:
                    tgt_local[t_idx] = tgt_global[t_idx]
                else:
                    tgt_local[t_idx] = quat_relative(tgt_global[t_idx], tgt_global[parent_t])

            # 根位移：源根全局位移经根对齐旋转后，目标根缩放只施加一次。
            root_align = alignment.get(root_target, quat_identity())
            root_translation = quat_rotate(root_align, src_positions[0])
            root_translation = root_translation * float(target.joints[root_target].scale)

            yield RetargetFrame(
                frame=frame,
                global_rotations=tgt_global,
                local_rotations=tgt_local,
                root_translation=root_translation,
                resolved=True,
            )
        except (ValueError, np.linalg.LinAlgError) as exc:
            skipped = RetargetFrame(
                frame=frame,
                global_rotations=np.tile(quat_identity(), (n_target, 1)),
                local_rotations=np.tile(quat_identity(), (n_target, 1)),
                root_translation=np.zeros(3, dtype=np.float64),
                resolved=False,
                error_code=E_POSE_UNRESOLVED,
            )
            skipped.__dict__["resolve_reason"] = str(exc)
            yield skipped


def retarget_clip(
    source: AnimDocument,
    target: AnimDocument,
    mapping: JointMapping,
    *,
    cancel: CancelCheck = None,
) -> RetargetResult:
    """对整段动画执行重定向（消费 :func:`retarget_frames`）。

    ``cancel`` 在每帧开始前检查；无法求解的帧以
    :data:`E_POSE_UNRESOLVED` 标记并跳过落盘通道。
    """

    frames: list[RetargetFrame] = []
    for frame_result in retarget_frames(source, target, mapping):
        if cancel is not None and cancel():
            raise PipelineError(E_CANCELLED, "重定向在逐帧检查点被取消",
                                [{"frame": frame_result.frame}])
        frames.append(frame_result)

    return RetargetResult(
        target_names=target.names,
        frames=frames,
        mapping_hash=mapping.content_hash(),
        source_version=source.version,
        target_version=target.version,
    )
