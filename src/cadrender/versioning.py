"""参数版本快照与提交边界（业务要求 5）。

每次编辑控制点或调整材质生成新版本号；快照内容不可变。预览任务只读
最新快照，精确任务启动时锁定快照。已提交分块按版本冻结，旧版本查询
返回 :class:`VersionError` (E_VER)，非法状态迁移返回 E_STATE。
"""
from __future__ import annotations

import copy
import hashlib
import json
from dataclasses import dataclass, field
from typing import Any, Dict, List, Tuple

import numpy as np

from .errors import E_STATE, E_VER, StateError, VersionError


def stable_hash(obj: Any) -> str:
    """对任意可 JSON 化结构计算确定性 SHA256。"""
    raw = json.dumps(obj, sort_keys=True, default=_json_default).encode()
    return hashlib.sha256(raw).hexdigest()


def _json_default(o: Any) -> Any:
    if isinstance(o, np.ndarray):
        return o.tolist()
    if isinstance(o, (np.floating, np.integer)):
        return o.item()
    raise TypeError(f"not serializable: {type(o)}")


@dataclass(frozen=True)
class Material:
    ambient: float = 0.25
    diffuse: float = 0.75
    specular: float = 0.4
    shininess: float = 24.0
    base_color: Tuple[float, float, float] = (0.62, 0.74, 0.88)

    def validate(self) -> None:
        for name in ("ambient", "diffuse", "specular", "shininess"):
            v = getattr(self, name)
            if not np.isfinite(v) or v < 0:
                from .errors import ParaError
                raise ParaError(f"材质参数 {name} 越界: {v}",
                                module="versioning", field=name)


@dataclass(frozen=True)
class SceneSnapshot:
    version: int
    scene_content: Dict[str, Any]      # 控制点/节点/权重等的可哈希描述
    material: Material
    content_hash: str
    parent_version: int
    change_note: str

    def to_meta(self) -> Dict[str, Any]:
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "material": self.material.__dict__,
            "parent_version": self.parent_version,
            "change_note": self.change_note,
        }


class VersionStore:
    """维护不可变快照序列与冻结分块集合。"""

    def __init__(self) -> None:
        self._snapshots: Dict[int, SceneSnapshot] = {}
        self._latest: int = 0
        self._frozen: Dict[int, set] = {}
        self._order: List[int] = []

    def initial(self, scene_content: Dict[str, Any],
                material: Material) -> SceneSnapshot:
        snap = SceneSnapshot(
            version=1,
            scene_content=copy.deepcopy(scene_content),
            material=material,
            content_hash=stable_hash(scene_content),
            parent_version=0,
            change_note="initial",
        )
        self._snapshots[1] = snap
        self._latest = 1
        self._order = [1]
        self._frozen[1] = set()
        return snap

    def commit(self, scene_content: Dict[str, Any], material: Material,
               note: str = "edit") -> SceneSnapshot:
        material.validate()
        new_version = self._latest + 1
        snap = SceneSnapshot(
            version=new_version,
            scene_content=copy.deepcopy(scene_content),
            material=material,
            content_hash=stable_hash(scene_content),
            parent_version=self._latest,
            change_note=note,
        )
        self._snapshots[new_version] = snap
        self._frozen[new_version] = set()
        self._order.append(new_version)
        self._latest = new_version
        return snap

    @property
    def latest_version(self) -> int:
        return self._latest

    def latest(self) -> SceneSnapshot:
        return self._snapshots[self._latest]

    def get(self, version: int) -> SceneSnapshot:
        if version not in self._snapshots:
            raise VersionError(
                f"请求版本 {version} 不存在，现存版本 {self._order}",
                module="versioning", field="version",
                context={"requested": version, "existing": list(self._order)},
            )
        return self._snapshots[version]

    def lock_for_precise(self, version: int) -> SceneSnapshot:
        """精确任务启动时锁定版本，拒绝非法迁移。"""
        if version not in self._snapshots:
            raise StateError(
                f"无法锁定不存在的版本 {version}",
                module="versioning", field="version",
                context={"requested": version},
            )
        return self._snapshots[version]

    def freeze_tile(self, version: int, tile_id: int) -> None:
        if version not in self._snapshots:
            raise StateError(f"未知版本 {version} 无法冻结分块",
                            module="versioning")
        self._frozen[version].add(tile_id)

    def frozen_tiles(self, version: int) -> set:
        if version not in self._frozen:
            raise VersionError(
                f"按版本 {version} 查询冻结分块失败，现存版本 "
                f"{self._order}",
                module="versioning", field="version",
                context={"requested": version, "existing": list(self._order)},
            )
        return set(self._frozen[version])

    def assert_editable(self, version: int, tile_id: int) -> None:
        """编辑只能作用于尚未提交（冻结）的分块。"""
        if version in self._frozen and tile_id in self._frozen[version]:
            raise VersionError(
                f"分块 {tile_id} 在版本 {version} 已冻结，编辑只能作用于"
                f"新版本（现存最新 {self._latest}）",
                module="versioning", field="tile_id",
                context={"version": version, "tile_id": tile_id,
                         "latest": self._latest},
            )

    def migrate(self, from_version: int, to_version: int) -> None:
        """显式状态迁移校验：只允许沿父子链向前迁移。"""
        if from_version == to_version:
            return
        if to_version not in self._snapshots:
            raise StateError(f"非法迁移：目标版本 {to_version} 不存在",
                            module="versioning")
        cur = to_version
        seen = set()
        while cur != 0 and cur not in seen:
            seen.add(cur)
            if cur == from_version:
                return
            cur = self._snapshots[cur].parent_version
        raise StateError(
            f"非法迁移：{from_version} -> {to_version} 不在同一条版本链上",
            module="versioning",
            context={"from": from_version, "to": to_version},
        )
