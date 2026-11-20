"""骨架 / 动画 / 重定向结果的数据模型。

内部统一表示始终按 v3 能力承载字段；低版本缺失字段在升级层填充，
并在 :attr:`AnimDocument.provenance` 中标注来源（native / derived）。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .math3d import quat_identity

SUPPORTED_VERSIONS = (1, 2, 3)


@dataclass
class JointDef:
    """单个关节的绑定信息。"""

    name: str
    parent: int | None
    bind_matrix: np.ndarray | None = None      # 4x4 绑定姿态局部参考矩阵（v3）
    scale: float = 1.0                          # 骨骼长度缩放（v3）
    aliases: list[str] = field(default_factory=list)

    def clone(self) -> "JointDef":
        return JointDef(
            name=self.name,
            parent=self.parent,
            bind_matrix=None if self.bind_matrix is None else self.bind_matrix.copy(),
            scale=self.scale,
            aliases=list(self.aliases),
        )


@dataclass
class AnimDocument:
    """动画文件解析后的内部文档（统一 v3 能力集）。"""

    version: int
    joints: list[JointDef]
    local_rotations: np.ndarray                  # shape (F, N, 4)，[w,x,y,z]
    local_translations: np.ndarray               # shape (F, N, 3)
    frame_rate: float = 30.0
    source_path: str | None = None
    provenance: dict[str, dict[str, str]] = field(default_factory=dict)
    metadata: dict[str, str] = field(default_factory=dict)

    # ---- 便捷访问器 ----------------------------------------------------
    @property
    def frame_count(self) -> int:
        return int(self.local_rotations.shape[0])

    @property
    def joint_count(self) -> int:
        return len(self.joints)

    @property
    def names(self) -> list[str]:
        return [j.name for j in self.joints]

    @property
    def parents(self) -> list[int | None]:
        return [j.parent for j in self.joints]

    @property
    def bind_matrices(self) -> np.ndarray:
        out = np.zeros((self.joint_count, 4, 4), dtype=np.float64)
        for i, joint in enumerate(self.joints):
            out[i] = np.eye(4) if joint.bind_matrix is None else joint.bind_matrix
        return out

    @property
    def scales(self) -> np.ndarray:
        return np.array([j.scale for j in self.joints], dtype=np.float64)

    @property
    def alias_map(self) -> dict[str, list[str]]:
        return {j.name: list(j.aliases) for j in self.joints}

    def index_of(self, name: str) -> int:
        return self.names.index(name)

    def frame_local_rotations(self, frame: int) -> np.ndarray:
        return self.local_rotations[frame]

    def frame_local_translations(self, frame: int) -> np.ndarray:
        return self.local_translations[frame]

    def clone(self) -> "AnimDocument":
        return AnimDocument(
            version=self.version,
            joints=[j.clone() for j in self.joints],
            local_rotations=self.local_rotations.copy(),
            local_translations=self.local_translations.copy(),
            frame_rate=self.frame_rate,
            source_path=self.source_path,
            provenance={k: dict(v) for k, v in self.provenance.items()},
            metadata=dict(self.metadata),
        )


@dataclass
class RetargetFrame:
    """单帧重定向输出。"""

    frame: int
    global_rotations: np.ndarray                 # (Nt, 4)
    local_rotations: np.ndarray                  # (Nt, 4)
    root_translation: np.ndarray                 # (3,)
    resolved: bool = True
    error_code: str | None = None


@dataclass
class RetargetResult:
    """整段重定向结果与帧级状态。"""

    target_names: list[str]
    frames: list[RetargetFrame]
    mapping_hash: str
    source_version: int
    target_version: int

    @property
    def frame_count(self) -> int:
        return len(self.frames)

    @property
    def resolved_frames(self) -> list[RetargetFrame]:
        return [f for f in self.frames if f.resolved]

    @property
    def skipped_frames(self) -> list[RetargetFrame]:
        return [f for f in self.frames if not f.resolved]

    def global_rotation_array(self) -> np.ndarray:
        """仅堆叠已求解帧，形状 (R, N, 4)。"""
        return np.stack([f.global_rotations for f in self.frames if f.resolved], axis=0)


def empty_rotation_channels(frame_count: int, joint_count: int) -> np.ndarray:
    arr = np.zeros((frame_count, joint_count, 4), dtype=np.float64)
    arr[..., :] = quat_identity()
    return arr
