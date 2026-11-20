"""固定视角离屏渲染与增量重渲。

渲染完全由 NumPy 完成（不依赖 GPU/显卡驱动），再通过 :class:`PySide6.QtGui.QImage`
封装成图像缓冲。视角固定为正射俯视，参数固定时：

* 全量渲染与增量重渲结果逐字节一致（同一纯函数 + 仅覆盖失效块像素区域）；
* 连续两次渲染结果逐字节一致；
* 颜色缓冲与误差热力缓冲按 chunk 分块缓存，未失效块直接复用缓存瓦片。
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional

import numpy as np
from PySide6.QtGui import QImage

from .dependency import NodeKind
from .engine import TerrainEngine
from .hashing import sha256_array, sha256_bytes


# 16 色调色板（确定性，材质索引 -> RGB）
PALETTE = np.array(
    [
        (92, 168, 87),
        (137, 119, 93),
        (180, 160, 110),
        (90, 120, 170),
        (200, 200, 205),
        (150, 90, 70),
        (70, 140, 120),
        (180, 130, 60),
        (110, 180, 200),
        (160, 100, 160),
        (120, 120, 80),
        (210, 180, 140),
        (80, 90, 110),
        (150, 200, 90),
        (190, 190, 120),
        (100, 100, 100),
    ],
    dtype=np.uint8,
)


@dataclass
class RenderSummary:
    seq: int
    invalidated_count: int
    reused_count: int
    seam_status: str
    color_hash: str
    heat_hash: str
    total_chunks: int
    invalidated: list[tuple[int, int]]
    reused: list[tuple[int, int]]

    def to_dict(self) -> dict:
        return {
            "seq": self.seq,
            "invalidated_count": self.invalidated_count,
            "reused_count": self.reused_count,
            "seam_status": self.seam_status,
            "color_hash": self.color_hash,
            "heat_hash": self.heat_hash,
            "total_chunks": self.total_chunks,
            "invalidated": [list(c) for c in self.invalidated],
            "reused": [list(c) for c in self.reused],
        }


class OffscreenRenderer:
    def __init__(self, engine: TerrainEngine) -> None:
        self.engine = engine
        self.color_buffer = np.zeros((engine.height, engine.width, 3), dtype=np.uint8)
        self.heat_buffer = np.zeros((engine.height, engine.width, 3), dtype=np.uint8)
        # 每 chunk 瓦片缓存（颜色/热力），键为 (cx, cy)
        self._color_tiles: dict[tuple[int, int], np.ndarray] = {}
        self._heat_tiles: dict[tuple[int, int], np.ndarray] = {}
        # 缓存瓦片对应的输入哈希证据（高程/材质/LOD），复用前必须验证
        self._tile_input_hash: dict[tuple[int, int], tuple[str, str, int]] = {}
        self.last_summary: Optional[RenderSummary] = None
        # 固定热力刻度：来自引擎初始粗糙度，不随补丁变化
        self._heat_scale = engine.heat_scale

    # ------------------------------------------------------------------ #
    # 单块纯函数渲染（全量/增量共享，逐位一致）
    # ------------------------------------------------------------------ #
    def render_chunk_color(self, cx: int, cy: int) -> np.ndarray:
        engine = self.engine
        chunk = engine.chunks[(cx, cy)]
        y0, y1, x0, x1 = chunk.y0, chunk.y1, chunk.x0, chunk.x1
        block_h = engine.elevations[y0:y1, x0:x1].astype(np.float64)
        block_n = engine.normals[y0:y1, x0:x1]
        block_w = engine.blend[y0:y1, x0:x1]  # (h, w, palette)

        # 材质双线性权重 -> 调色板颜色
        palette = PALETTE[: engine.material_palette_size].astype(np.float64)
        base = block_w @ palette  # (h, w, 3)

        # 法线固定方向光照（确定性）
        light = np.array([0.35, 0.45, 0.82], dtype=np.float64)
        light /= np.linalg.norm(light)
        shade = np.clip(block_n.astype(np.float64) @ light, 0.25, 1.0)[..., None]

        # 高程热力叠加点（按全局高程范围归一化）
        h_min, h_max = engine.display_range
        span = max(1e-9, h_max - h_min)
        t = ((block_h - h_min) / span)[..., None]
        heat_tint = np.concatenate(
            [t * 255.0, np.zeros_like(t), (1.0 - t) * 255.0], axis=-1
        )
        color = base * shade * 0.82 + heat_tint * 0.18
        return np.clip(color, 0, 255).astype(np.uint8)

    def render_chunk_heat(self, cx: int, cy: int) -> np.ndarray:
        """误差热力：局部粗糙度（LOD 误差代理）映射到蓝-红刻度。"""
        engine = self.engine
        chunk = engine.chunks[(cx, cy)]
        block_r = engine.roughness[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
        t = np.clip(block_r / self._heat_scale, 0.0, 1.0)
        rgb = np.stack(
            [t * 255.0, np.zeros_like(t), (1.0 - t) * 255.0], axis=-1
        )
        return np.clip(rgb, 0, 255).astype(np.uint8)

    # ------------------------------------------------------------------ #
    # 全量 / 增量渲染
    # ------------------------------------------------------------------ #
    def render_full(self, seq: Optional[int] = None) -> RenderSummary:
        for cx, cy in self.engine.chunks:
            self._stamp_tile(cx, cy, self.render_chunk_color(cx, cy), self.render_chunk_heat(cx, cy))
        return self._summarize(
            seq if seq is not None else self.engine.version,
            invalidated=sorted(self.engine.chunks.keys()),
            reused=[],
            force_seam_status=None,
        )

    def render_incremental(self, report=None) -> RenderSummary:
        """只重渲失效块区域，其余块直接复用缓存瓦片。"""
        report = report or self.engine.last_report
        invalidation = report.invalidation
        render_set = set(invalidation.invalid[NodeKind.RENDER])
        # 保守补集：缺缓存、或输入哈希证据对不上（跨批次陈旧瓦片）的块一律重渲
        for key, chunk in self.engine.chunks.items():
            evidence = self._tile_input_hash.get(key)
            current = (chunk.elev_hash, chunk.material_hash, chunk.target_level)
            if (
                key not in self._color_tiles
                or key not in self._heat_tiles
                or evidence != current
            ):
                render_set.add(key)

        reused = []
        for key in sorted(self.engine.chunks):
            if key in render_set:
                cx, cy = key
                self._stamp_tile(
                    cx, cy, self.render_chunk_color(cx, cy), self.render_chunk_heat(cx, cy)
                )
            else:
                # 复用：把缓存瓦片重新盖回缓冲（证明逐字节一致）
                rcx, rcy = key
                self._stamp_tile(
                    rcx, rcy, self._color_tiles[key], self._heat_tiles[key]
                )
                reused.append(key)
        return self._summarize(
            report.seq,
            invalidated=sorted(render_set),
            reused=sorted(reused),
            force_seam_status=None,
            report=report,
        )

    def _stamp_tile(
        self, cx: int, cy: int, color_tile: np.ndarray, heat_tile: np.ndarray
    ) -> None:
        chunk = self.engine.chunks[(cx, cy)]
        y0, y1, x0, x1 = chunk.y0, chunk.y1, chunk.x0, chunk.x1
        self.color_buffer[y0:y1, x0:x1] = color_tile
        self.heat_buffer[y0:y1, x0:x1] = heat_tile
        self._color_tiles[(cx, cy)] = color_tile.copy()
        self._heat_tiles[(cx, cy)] = heat_tile.copy()
        self._tile_input_hash[(cx, cy)] = (
            chunk.elev_hash,
            chunk.material_hash,
            chunk.target_level,
        )

    def _summarize(
        self,
        seq: int,
        invalidated: list[tuple[int, int]],
        reused: list[tuple[int, int]],
        force_seam_status: Optional[str],
        report=None,
    ) -> RenderSummary:
        if force_seam_status is not None:
            seam_status = force_seam_status
        elif report is not None and report.crack is not None:
            seam_status = "E_CRACK_UNRESOLVED"
        elif report is not None and report.seam_report:
            open_seams = [r for r in report.seam_report if not r["closed"]]
            seam_status = "open" if open_seams else "closed"
        else:
            seam_status = "closed"
        summary = RenderSummary(
            seq=int(seq),
            invalidated_count=len(invalidated),
            reused_count=len(reused),
            seam_status=seam_status,
            color_hash=sha256_array(self.color_buffer),
            heat_hash=sha256_array(self.heat_buffer),
            total_chunks=len(self.engine.chunks),
            invalidated=invalidated,
            reused=reused,
        )
        self.last_summary = summary
        return summary

    # ------------------------------------------------------------------ #
    # QImage 导出与缓冲释放
    # ------------------------------------------------------------------ #
    def color_qimage(self) -> QImage:
        return self._to_qimage(self.color_buffer)

    def heat_qimage(self) -> QImage:
        return self._to_qimage(self.heat_buffer)

    @staticmethod
    def _to_qimage(buffer: np.ndarray) -> QImage:
        rgb = np.ascontiguousarray(buffer)
        h, w, _ = rgb.shape
        image = QImage(rgb.data, w, h, 3 * w, QImage.Format.Format_RGB888)
        return image.copy()  # 拷贝脱离 numpy 内存生命周期

    def release_buffers(self) -> None:
        """协作式取消后释放离屏缓冲与瓦片缓存。"""
        self._color_tiles.clear()
        self._heat_tiles.clear()
        self._tile_input_hash.clear()
        self.color_buffer = np.zeros_like(self.color_buffer)
        self.heat_buffer = np.zeros_like(self.heat_buffer)
        self.last_summary = None

    def buffer_bytes_hash(self) -> tuple[str, str]:
        return sha256_bytes(self.color_buffer.tobytes()), sha256_bytes(
            self.heat_buffer.tobytes()
        )
