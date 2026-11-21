"""不可变版本化发布与查询隔离。

- 编码帧、误差摘要与参数集组成一个不可变 Version
- 重算在独立数据结构（VersionBuilder）中完成，再一次性整体替换发布
- 查询始终命中某个完整版本，回带版本哈希
- 版本哈希与发布清单不匹配时返回 E_VERSION_STALE，不返回任何帧

版本哈希 = SHA-256(规范 JSON(清单) || 逐帧编码哈希链)，
只依赖内容，不依赖时间戳或文件大小。
"""

from __future__ import annotations

import os
from dataclasses import dataclass

import numpy as np

from .errors import VersionStaleError
from .hashing import array_hash, dict_hash


@dataclass(frozen=True)
class FramePayload:
    frame_index: int
    codes: np.ndarray
    source_hash: str
    stage_hashes: dict
    max_diff_codes: float
    over_tolerance: bool
    bound_summary: dict

    def content_hash(self) -> str:
        return array_hash(self.codes)


@dataclass(frozen=True)
class Version:
    version_id: int
    version_hash: str
    config_factors: dict
    header: dict
    frames: tuple[FramePayload, ...]
    frame_hashes: tuple[str, ...]
    affected_frames: tuple[int, ...]
    reused_frames: tuple[int, ...]

    def manifest(self) -> dict:
        return {
            "version_id": self.version_id,
            "config_factors": self.config_factors,
            "header": self.header,
            "frame_hashes": list(self.frame_hashes),
            "affected_frames": list(self.affected_frames),
            "reused_frames": list(self.reused_frames),
            "frame_count": len(self.frames),
        }


def build_version_hash(manifest: dict, frame_hashes: list[str]) -> str:
    chain_seed = dict_hash(manifest)
    digest = chain_seed
    for index, frame_hash in enumerate(frame_hashes):
        digest = dict_hash(
            {"prev": digest, "frame": index, "content": frame_hash}
        )
    return digest


class VersionBuilder:
    """重算期间的独立可变结构，未发布前对外不可见。"""

    def __init__(self, version_id: int, config, header, affected, reused):
        self.version_id = version_id
        self._config = config
        self._header = header
        self._affected = tuple(affected)
        self._reused = tuple(reused)
        self._frames: dict[int, FramePayload] = {}

    def add_frame(self, payload: FramePayload) -> None:
        if payload.frame_index in self._frames:
            raise ValueError(f"帧 {payload.frame_index} 已加入本版本")
        self._frames[payload.frame_index] = payload

    def publish(self) -> Version:
        ordered = [self._frames[i] for i in sorted(self._frames)]
        frame_hashes = tuple(item.content_hash() for item in ordered)
        manifest = {
            "version_id": self.version_id,
            "config_factors": self._config.factor_dict(),
            "header": self._header.as_dict(),
            "frame_hashes": list(frame_hashes),
            "affected_frames": list(self._affected),
            "reused_frames": list(self._reused),
            "frame_count": len(ordered),
        }
        version_hash = build_version_hash(manifest, list(frame_hashes))
        return Version(
            version_id=self.version_id,
            version_hash=version_hash,
            config_factors=manifest["config_factors"],
            header=manifest["header"],
            frames=tuple(ordered),
            frame_hashes=frame_hashes,
            affected_frames=self._affected,
            reused_frames=self._reused,
        )


class VersionStore:
    """保存不可变版本；指针切换是唯一的发布动作。"""

    def __init__(self):
        self._versions: dict[str, Version] = {}
        self._current_hash: str | None = None
        self._history: list[str] = []

    def publish(self, version: Version) -> str:
        self._verify_version(version)
        self._versions[version.version_hash] = version
        self._current_hash = version.version_hash
        self._history.append(version.version_hash)
        return version.version_hash

    def current(self) -> Version | None:
        if self._current_hash is None:
            return None
        return self._versions[self._current_hash]

    def get(self, version_hash: str | None = None) -> Version:
        target_hash = version_hash or self._current_hash
        if target_hash is None or target_hash not in self._versions:
            raise VersionStaleError(
                "请求的版本哈希不存在或已过期", version=target_hash
            )
        version = self._versions[target_hash]
        self._verify_version(version)
        return version

    def query_frames(
        self, frame_indices: list[int] | None = None, version_hash: str | None = None
    ) -> dict:
        """查询入口：校验通过才返回完整命中版本的数据。"""
        version = self.get(version_hash)
        indices = (
            range(len(version.frames))
            if frame_indices is None
            else frame_indices
        )
        return {
            "version_hash": version.version_hash,
            "frames": [version.frames[i] for i in indices],
        }

    def tamper(self, version_hash: str) -> None:
        """测试辅助：制造清单/内容不一致以触发 E_VERSION_STALE。"""
        version = self._versions[version_hash]
        broken = FramePayload(
            frame_index=0,
            codes=version.frames[0].codes + 1,
            source_hash=version.frames[0].source_hash,
            stage_hashes=version.frames[0].stage_hashes,
            max_diff_codes=version.frames[0].max_diff_codes,
            over_tolerance=version.frames[0].over_tolerance,
            bound_summary=version.frames[0].bound_summary,
        )
        frames = (broken,) + version.frames[1:]
        self._versions[version_hash] = Version(
            version_id=version.version_id,
            version_hash=version.version_hash,
            config_factors=version.config_factors,
            header=version.header,
            frames=frames,
            frame_hashes=version.frame_hashes,
            affected_frames=version.affected_frames,
            reused_frames=version.reused_frames,
        )

    @staticmethod
    def _verify_version(version: Version) -> None:
        actual_hashes = [item.content_hash() for item in version.frames]
        if actual_hashes != list(version.frame_hashes):
            raise VersionStaleError(
                "版本帧哈希与发布清单不一致", version=version.version_hash
            )
        manifest = version.manifest()
        rebuilt = build_version_hash(manifest, actual_hashes)
        if rebuilt != version.version_hash:
            raise VersionStaleError(
                "版本哈希重算与发布值不匹配", version=version.version_hash
            )

    def history(self) -> list[str]:
        return list(self._history)


def atomic_write_json(path: str, payload: str) -> None:
    """临时文件 + fsync + 原子重命名。"""
    tmp_path = f"{path}.tmp"
    with open(tmp_path, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp_path, path)

