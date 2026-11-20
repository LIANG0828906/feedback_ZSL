"""跨格式版本兼容层。

* :func:`upgrade_to_v3` 把 v1/v2 文档在内存中升级为统一 v3 表示，
  绑定姿态缺失时由首帧局部变换推导，别名缺失时按规范化名生成，
  所有推导字段在 ``provenance`` 标注来源；
* :func:`downgrade` 导出旧版本时，无法表达的字段以
  :data:`~retarget_pipeline.errors.E_VERSION_LOSS` 汇总（字段清单+受影响关节），
  绝不静默丢弃缩放或绑定姿态信息。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .errors import E_VERSION_LOSS, PipelineError
from .math3d import make_transform
from .model import AnimDocument
from .naming import normalize_name

DERIVED_BIND = "derived:first_frame"
NATIVE_BIND = "native:bind_matrix"
DERIVED_ALIAS = "derived:normalized_name"
NATIVE_ALIAS = "native:alias_table"
DERIVED_PARENT = "derived:v1_linear_chain"
NATIVE_PARENT = "native:parent_table"
DEFAULT_SCALE = 1.0


@dataclass
class VersionLossReport:
    target_version: int
    lost_fields: dict[str, list[str]] = field(default_factory=dict)

    def add(self, field_name: str, joints: list[str]) -> None:
        if joints:
            self.lost_fields[field_name] = joints

    def to_error(self) -> PipelineError | None:
        if not self.lost_fields:
            return None
        locations = [
            {"field": field_name, "joints": joints, "joint_count": len(joints)}
            for field_name, joints in sorted(self.lost_fields.items())
        ]
        return PipelineError(
            E_VERSION_LOSS,
            f"降级到 v{self.target_version} 存在无法表达的字段: "
            f"{', '.join(sorted(self.lost_fields))}",
            locations,
        )


def upgrade_to_v3(doc: AnimDocument) -> AnimDocument:
    """返回升级后的 v3 文档（不修改入参），并填充 provenance。"""

    out = doc.clone()
    out.version = 3
    provenance = out.provenance

    for idx, joint in enumerate(out.joints):
        record = provenance.setdefault(joint.name, {})

        # 层级来源
        record.setdefault(
            "parent",
            DERIVED_PARENT if doc.version == 1 else NATIVE_PARENT,
        )

        # 绑定姿态参考矩阵
        if joint.bind_matrix is None:
            first_rot = out.local_rotations[0, idx]
            first_trans = out.local_translations[0, idx]
            joint.bind_matrix = make_transform(
                np.eye(3), first_trans
            )
            # 首帧局部旋转写入参考矩阵旋转块
            from .math3d import quat_to_matrix

            joint.bind_matrix[:3, :3] = quat_to_matrix(first_rot)
            record["bind_matrix"] = DERIVED_BIND
        else:
            record["bind_matrix"] = NATIVE_BIND

        # 别名
        if not joint.aliases:
            joint.aliases = [normalize_name(joint.name)]
            record["aliases"] = DERIVED_ALIAS
        else:
            record["aliases"] = NATIVE_ALIAS

        # 缩放：旧版本没有缩放概念，统一为 1.0 并标注
        record.setdefault("scale", "default:1.0")
        joint.scale = float(joint.scale) if joint.scale > 0 else DEFAULT_SCALE

    out.metadata["upgraded_from"] = str(doc.version)
    return out


def plan_downgrade(doc: AnimDocument, target_version: int) -> VersionLossReport:
    """计算降级到指定版本会丢失哪些字段（不修改文档）。"""

    report = VersionLossReport(target_version=target_version)
    if target_version >= 3:
        return report

    names = doc.names
    if target_version <= 1:
        # v1 无法表达层级与平移
        report.add("parent", names)
        moved = [j for j in names if not np.allclose(
            doc.local_translations[:, doc.index_of(j)], 0.0, atol=1e-12)]
        report.add("translations", moved or names)

    # v1/v2 都无法表达 v3 三件套
    scaled = [j.name for j in doc.joints if abs(j.scale - 1.0) > 1e-12]
    report.add("scale", scaled)
    report.add("bind_matrix", [j.name for j in doc.joints if j.bind_matrix is not None])
    alias_joints = [j.name for j in doc.joints if j.aliases]
    report.add("aliases", alias_joints)
    return report


def downgrade(doc: AnimDocument, target_version: int, *, accept_loss: bool = False) -> tuple[AnimDocument, VersionLossReport]:
    """降级导出。

    :param accept_loss: 为 ``False``（默认）时，一旦存在字段损失即抛
        :data:`E_VERSION_LOSS`；为 ``True`` 时执行降级并返回损失报告，
        由调用方把报告写入追溯日志，保证不静默丢弃。
    """

    report = plan_downgrade(doc, target_version)
    error = report.to_error()
    if error is not None and not accept_loss:
        raise error

    out = doc.clone()
    out.version = target_version
    for joint in out.joints:
        if target_version < 3:
            joint.bind_matrix = None
            joint.aliases = []
            joint.scale = 1.0
        if target_version == 1:
            joint.parent = None
    if target_version == 1:
        out.local_translations = np.zeros_like(out.local_translations)
    out.metadata["downgraded_from"] = str(doc.version)
    return out, report
